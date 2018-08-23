// Copyright 2016-2018 Authors of Cilium
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package ipcache

import (
	"fmt"
	"net"
	"time"

	"github.com/cilium/cilium/pkg/bpf"
	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/identity"
	"github.com/cilium/cilium/pkg/ipcache"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	ipcacheMap "github.com/cilium/cilium/pkg/maps/ipcache"
	"github.com/cilium/cilium/pkg/node"

	"github.com/sirupsen/logrus"
)

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, "datapath-ipcache")

// BPFListener implements the ipcache.IPIdentityMappingBPFListener
// interface with an IPCache store that is backed by BPF maps.
type BPFListener struct {
	// bpfMap is the BPF map that this listener will update when events are
	// received from the IPCache.
	bpfMap *ipcacheMap.Map
}

func newListener(m *ipcacheMap.Map) *BPFListener {
	return &BPFListener{
		bpfMap: m,
	}
}

// NewListener returns a new listener to push IPCache entries into BPF maps.
func NewListener() *BPFListener {
	return newListener(ipcacheMap.IPCache)
}

// OnIPIdentityCacheChange is called whenever there is a change of state in the
// IPCache (pkg/ipcache).
// TODO (FIXME): GH-3161.
//
// 'oldIPIDPair' is ignored here, because in the BPF maps an update for the
// IP->ID mapping will replace any existing contents; knowledge of the old pair
// is not required to upsert the new pair.
func (l *BPFListener) OnIPIdentityCacheChange(modType ipcache.CacheModification, cidr net.IPNet,
	oldHostIP, newHostIP net.IP, oldID *identity.NumericIdentity, newID identity.NumericIdentity) {
	scopedLog := log.WithFields(logrus.Fields{
		logfields.IPAddr:       cidr,
		logfields.Identity:     newID,
		logfields.Modification: modType,
	})

	scopedLog.Debug("Daemon notified of IP-Identity cache state change")

	// TODO - see if we can factor this into an interface under something like
	// pkg/datapath instead of in the daemon directly so that the code is more
	// logically located.

	// Update BPF Maps.

	key := ipcacheMap.NewKey(cidr.IP, cidr.Mask)

	switch modType {
	case ipcache.Upsert:
		value := ipcacheMap.RemoteEndpointInfo{
			SecurityIdentity: uint32(newID),
		}

		if newHostIP != nil {
			// If the hostIP is specified and it doesn't point to
			// the local host, then the ipcache should be populated
			// with the hostIP so that this traffic can be guided
			// to a tunnel endpoint destination.
			externalIP := node.GetExternalIPv4()
			if ip4 := newHostIP.To4(); ip4 != nil && !ip4.Equal(externalIP) {
				copy(value.TunnelEndpoint[:], ip4)
			}
		}
		err := l.bpfMap.Update(&key, &value)
		if err != nil {
			scopedLog.WithError(err).WithFields(logrus.Fields{"key": key.String(),
				"value": value.String()}).
				Warning("unable to update bpf map")
		}
	case ipcache.Delete:
		err := l.bpfMap.Delete(&key)
		if err != nil {
			scopedLog.WithError(err).WithFields(logrus.Fields{"key": key.String()}).
				Warning("unable to delete from bpf map")
		}
	default:
		scopedLog.Warning("cache modification type not supported")
	}
}

// updateStaleEntriesFunction returns a DumpCallback that will update the
// specified "keysToRemove" map with entries that exist in the BPF map which
// do not exist in the in-memory ipcache.
//
// Must be called while holding ipcache.IPIdentityCache.Lock for reading.
func updateStaleEntriesFunction(keysToRemove map[string]*ipcacheMap.Key) bpf.DumpCallback {
	return func(key bpf.MapKey, value bpf.MapValue) {
		k := key.(*ipcacheMap.Key)
		keyToIP := k.String()

		// Don't RLock as part of the same goroutine.
		if i, exists := ipcache.IPIdentityCache.LookupByPrefixRLocked(keyToIP); !exists {
			switch i.Source {
			case ipcache.FromKVStore, ipcache.FromAgentLocal:
				// Cannot delete from map during callback because DumpWithCallback
				// RLocks the map.
				keysToRemove[keyToIP] = k
			}
		}
	}
}

// garbageCollect implements GC of the ipcache map.
//
//   Periodically sweep through every element in the BPF map and check it
//   against the in-memory copy of the map. If it doesn't exist in memory,
//   delete the entry.
//
// Returns an error if garbage collection failed to occur.
func (l *BPFListener) garbageCollect() error {
	log.Debug("Running garbage collection for BPF IPCache")

	// Since controllers run asynchronously, need to make sure
	// IPIdentityCache is not being updated concurrently while we do
	// GC;
	ipcache.IPIdentityCache.RLock()
	defer ipcache.IPIdentityCache.RUnlock()

	keysToRemove := map[string]*ipcacheMap.Key{}
	if err := l.bpfMap.DumpWithCallback(updateStaleEntriesFunction(keysToRemove)); err != nil {
		return fmt.Errorf("error dumping ipcache BPF map: %s", err)
	}

	// Remove all keys which are not in in-memory cache from BPF map
	// for consistency.
	for _, k := range keysToRemove {
		log.WithFields(logrus.Fields{logfields.BPFMapKey: k}).
			Debug("deleting from ipcache BPF map")
		if err := l.bpfMap.Delete(k); err != nil {
			return fmt.Errorf("error deleting key %s from ipcache BPF map: %s", k, err)
		}
	}
	return nil
}

// OnIPIdentityCacheGC spawns a controller which synchronizes the BPF IPCache Map
// with the in-memory IP-Identity cache.
func (l *BPFListener) OnIPIdentityCacheGC() {
	// This controller ensures that the in-memory IP-identity cache is in-sync
	// with the BPF map on disk. These can get out of sync if the cilium-agent
	// is offline for some time, as the maps persist on the BPF filesystem.
	// In the case that there is some loss of event history in the key-value
	// store (e.g., compaction in etcd), we cannot rely upon the key-value store
	// fully to give us the history of all events. As such, periodically check
	// for inconsistencies in the data-path with that in the agent to ensure
	// consistent state.
	controller.NewManager().UpdateController("ipcache-bpf-garbage-collection",
		controller.ControllerParams{
			DoFunc:      l.garbageCollect,
			RunInterval: 5 * time.Minute,
		},
	)
}