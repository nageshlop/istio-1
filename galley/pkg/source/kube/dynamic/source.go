// Copyright 2018 Istio Authors
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

package dynamic

import (
	"fmt"
	"sync"
	"time"

	"istio.io/istio/pkg/listwatch"
	"istio.io/istio/pkg/servicemesh/controller"

	"istio.io/istio/galley/pkg/runtime"
	"istio.io/istio/galley/pkg/runtime/resource"
	"istio.io/istio/galley/pkg/source/kube/dynamic/converter"
	"istio.io/istio/galley/pkg/source/kube/log"
	sourceSchema "istio.io/istio/galley/pkg/source/kube/schema"
	"istio.io/istio/galley/pkg/source/kube/stats"
	"istio.io/istio/galley/pkg/source/kube/tombstone"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8sRuntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/cache"
)

var _ runtime.Source = &source{}

// source is a simplified client interface for listening/getting Kubernetes resources in an unstructured way.
type source struct {
	// Lock for changing the running state of the source
	stateLock sync.Mutex

	cfg *converter.Config

	spec sourceSchema.ResourceSpec

	watchedNamespaces []string
	resyncPeriod      time.Duration

	// The dynamic resource interface for accessing custom resources dynamically.
	resourceClient dynamic.NamespaceableResourceInterface

	// stopCh is used to quiesce the background activity during shutdown
	stopCh chan struct{}

	// SharedIndexInformer for watching/caching resources
	informer cache.SharedIndexInformer

	handler resource.EventHandler

	mrc controller.MemberRollController
}

// New returns a new instance of a dynamic source for the given schema.
func New(
	client dynamic.Interface, watchedNamespaces []string, resyncPeriod time.Duration,
	mrc controller.MemberRollController, spec sourceSchema.ResourceSpec,
	cfg *converter.Config) (runtime.Source, error) {

	gv := spec.GroupVersion()
	log.Scope.Debugf("Creating a new dynamic resource source for: name='%s', gv:'%v'",
		spec.Singular, gv)

	resourceClient := client.Resource(gv.WithResource(spec.Plural))

	return &source{
		spec:              spec,
		cfg:               cfg,
		watchedNamespaces: watchedNamespaces,
		resyncPeriod:      resyncPeriod,
		mrc:               mrc,
		resourceClient:    resourceClient,
	}, nil
}

// Start the source. This will commence listening and dispatching of events.
func (s *source) Start(handler resource.EventHandler) error {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()

	if s.stopCh != nil {
		return fmt.Errorf("already synchronizing resources: name='%s', gv='%v'",
			s.spec.Singular, s.spec.GroupVersion())
	}
	if handler == nil {
		return fmt.Errorf("invalid event handler")
	}

	log.Scope.Debugf("Starting source for %s(%v)", s.spec.Singular, s.spec.GroupVersion())

	s.stopCh = make(chan struct{})
	s.handler = handler

	mlw := listwatch.MultiNamespaceListerWatcher(s.watchedNamespaces, func(namespace string) cache.ListerWatcher {
		return &cache.ListWatch{
			ListFunc: func(options metav1.ListOptions) (k8sRuntime.Object, error) {
				return s.resourceClient.Namespace(namespace).List(options)
			},
			WatchFunc: func(options metav1.ListOptions) (watch.Interface, error) {
				options.Watch = true
				return s.resourceClient.Namespace(namespace).Watch(options)
			},
		}
	})
	if s.mrc != nil {
		s.mrc.Register(mlw)
	}

	s.informer = cache.NewSharedIndexInformer(
		mlw,
		&unstructured.Unstructured{},
		s.resyncPeriod,
		cache.Indexers{})

	s.informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) { s.handleEvent(resource.Added, obj) },
		UpdateFunc: func(old, new interface{}) {
			newRes := new.(*unstructured.Unstructured)
			oldRes := old.(*unstructured.Unstructured)
			if newRes.GetResourceVersion() == oldRes.GetResourceVersion() {
				// Periodic resync will send update events for all known resources.
				// Two different versions of the same resource will always have different RVs.
				return
			}
			s.handleEvent(resource.Updated, new)
		},
		DeleteFunc: func(obj interface{}) { s.handleEvent(resource.Deleted, obj) },
	})

	// Start CRD shared informer background process.
	go s.informer.Run(s.stopCh)

	// Send the an event after the cache syncs.
	go func() {
		_ = cache.WaitForCacheSync(s.stopCh, s.informer.HasSynced)
		handler(resource.FullSyncEvent)
	}()

	return nil
}

// Stop the source. This will stop publishing of events.
func (s *source) Stop() {
	s.stateLock.Lock()
	defer s.stateLock.Unlock()

	if s.stopCh == nil {
		log.Scope.Errorf("already stopped")
		return
	}

	close(s.stopCh)
	s.stopCh = nil
}

func (s *source) handleEvent(c resource.EventKind, obj interface{}) {
	object, ok := obj.(metav1.Object)
	if !ok {
		if object = tombstone.RecoverResource(obj); object == nil {
			// Tombstone recovery failed.
			return
		}
	}

	var u *unstructured.Unstructured
	if uns, ok := obj.(*unstructured.Unstructured); ok {
		u = uns

		// https://github.com/kubernetes/kubernetes/pull/63972
		// k8s machinery does not always preserve TypeMeta in list operations. Restore it
		// using aprior knowledge of the GVK for this source.
		u.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   s.spec.Group,
			Version: s.spec.Version,
			Kind:    s.spec.Kind,
		})
	}

	log.Scope.Debugf("Sending event: [%v] from: %s", c, s.spec.CanonicalResourceName())

	key := resource.FullNameFromNamespaceAndName(object.GetNamespace(), object.GetName())
	processEvent(s.cfg, s.spec, c, key, object.GetResourceVersion(), u, s.handler)
	stats.RecordEventSuccess()
}

// ConvertAndLog is a utility that invokes the converter and logs the success status.
func ConvertAndLog(cfg *converter.Config, spec sourceSchema.ResourceSpec, key resource.FullName,
	resourceVersion string, u *unstructured.Unstructured) ([]converter.Entry, error) {
	entries, err := spec.Converter(cfg, spec.Target, key, spec.Kind, u)
	if err != nil {
		log.Scope.Errorf("Unable to convert unstructured to proto: %s/%s: %v", key, resourceVersion, err)
		stats.RecordConverterResult(false, spec.Version, spec.Group, spec.Kind)
		return nil, err
	}
	stats.RecordConverterResult(true, spec.Version, spec.Group, spec.Kind)
	return entries, nil
}

// processEvent process the incoming message and convert it to event
func processEvent(cfg *converter.Config, spec sourceSchema.ResourceSpec, kind resource.EventKind, key resource.FullName,
	resourceVersion string, u *unstructured.Unstructured, handler resource.EventHandler) {

	entries, err := ConvertAndLog(cfg, spec, key, resourceVersion, u)
	if err != nil {
		return
	}

	if len(entries) == 0 {
		log.Scope.Debugf("Did not receive any entries from converter: kind=%v, key=%v, rv=%s",
			kind, key, resourceVersion)
		return
	}

	// TODO(nmittler): Will there ever be > 1 entries?
	entry := entries[0]

	var event resource.Event

	switch kind {
	case resource.Added, resource.Updated:
		event = resource.Event{
			Kind: kind,
			Entry: resource.Entry{
				ID: resource.VersionedKey{
					Key: resource.Key{
						Collection: spec.Target.Collection,
						FullName:   entry.Key,
					},
					Version: resource.Version(resourceVersion),
				},
				Item:     entry.Resource,
				Metadata: entry.Metadata,
			},
		}

	case resource.Deleted:
		event = resource.Event{
			Kind: kind,
			Entry: resource.Entry{
				ID: resource.VersionedKey{
					Key: resource.Key{
						Collection: spec.Target.Collection,
						FullName:   entry.Key,
					},
					Version: resource.Version(resourceVersion),
				},
			},
		}
	}

	log.Scope.Debugf("Dispatching source event: %v", event)
	handler(event)
}