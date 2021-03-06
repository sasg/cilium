// Copyright 2018 Authors of Cilium
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

package clustermesh

import (
	"fmt"

	"github.com/cilium/cilium/pkg/controller"
	"github.com/cilium/cilium/pkg/kvstore/store"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/option"
)

const (
	// configNotificationsChannelSize is the size of the channel used to
	// notify a clustermesh of configuration changes
	configNotificationsChannelSize = 512
)

// Configuration is the configuration that must be provided to
// NewClusterMesh()
type Configuration struct {
	// Name is the name of the remote cluster cache. This is for logging
	// purposes only
	Name string

	// ConfigDirectory is the path to the directory that will be watched for etcd
	// configuration files to appear
	ConfigDirectory string

	// NodeKeyCreator is the function used to create node instances as
	// nodes are being discovered in remote clusters
	NodeKeyCreator store.KeyCreator
}

// ClusterMesh is a cache of multiple remote clusters
type ClusterMesh struct {
	// conf is the configuration, it is immutable after NewClusterMesh()
	conf Configuration

	mutex         lock.RWMutex
	clusters      map[string]*remoteCluster
	controllers   *controller.Manager
	configWatcher *configDirectoryWatcher
}

// NewClusterMesh creates a new remote cluster cache based on the
// provided configuration
func NewClusterMesh(c Configuration) (*ClusterMesh, error) {
	cm := &ClusterMesh{
		conf:        c,
		clusters:    map[string]*remoteCluster{},
		controllers: controller.NewManager(),
	}

	w, err := createConfigDirectoryWatcher(c.ConfigDirectory, cm)
	if err != nil {
		return nil, fmt.Errorf("unable to create config directory watcher: %s", err)
	}

	cm.configWatcher = w

	controllerName := fmt.Sprintf("clustermesh-%s-config-fsnotify", c.Name)
	cm.controllers.UpdateController(controllerName,
		controller.ControllerParams{
			DoFunc: func() error { return cm.configWatcher.watch() },
		},
	)

	return cm, nil
}

// Close stops watching for remote cluster configuration files to appear and
// will close all connections to remote clusters
func (cm *ClusterMesh) Close() {
	cm.mutex.Lock()
	defer cm.mutex.Unlock()

	if cm.configWatcher != nil {
		cm.configWatcher.close()
	}

	for name, cluster := range cm.clusters {
		cluster.onRemove()
		delete(cm.clusters, name)
	}

	cm.controllers.RemoveAll()
}

func (cm *ClusterMesh) newRemoteCluster(name, path string) *remoteCluster {
	return &remoteCluster{
		name:        name,
		configPath:  path,
		mesh:        cm,
		changed:     make(chan bool, configNotificationsChannelSize),
		controllers: controller.NewManager(),
	}
}

func (cm *ClusterMesh) add(name, path string) {
	if name == option.Config.ClusterName {
		log.WithField(fieldClusterName, name).Debug("Ignoring configuration for own cluster")
		return
	}

	inserted := false
	cm.mutex.Lock()
	cluster, ok := cm.clusters[name]
	if !ok {
		cluster = cm.newRemoteCluster(name, path)
		cm.clusters[name] = cluster
		inserted = true
	}
	cm.mutex.Unlock()

	log.WithField(fieldClusterName, name).Debug("Remote cluster configuration added")

	if inserted {
		cluster.onInsert()
	} else {
		// signal a change in configuration
		cluster.changed <- true
	}
}

func (cm *ClusterMesh) remove(name string) {
	cm.mutex.Lock()
	if cluster, ok := cm.clusters[name]; ok {
		cluster.onRemove()
		delete(cm.clusters, name)
	}
	cm.mutex.Unlock()

	log.WithField(fieldClusterName, name).Debug("Remote cluster configuration removed")
}

// NumReadyClusters returns the number of remote clusters to which a connection
// has been established
func (cm *ClusterMesh) NumReadyClusters() int {
	cm.mutex.RLock()
	defer cm.mutex.RUnlock()

	nready := 0
	for _, cm := range cm.clusters {
		if cm.isReady() {
			nready++
		}
	}

	return nready
}
