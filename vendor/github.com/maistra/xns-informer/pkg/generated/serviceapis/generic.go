// Code generated by xns-informer-gen. DO NOT EDIT.

package serviceapis

import (
	"fmt"

	schema "k8s.io/apimachinery/pkg/runtime/schema"
	cache "k8s.io/client-go/tools/cache"
	v1alpha1 "sigs.k8s.io/service-apis/apis/v1alpha1"
)

// GenericInformer is type of SharedIndexInformer which will locate and delegate to other
// sharedInformers based on type
type GenericInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() cache.GenericLister
}
type genericInformer struct {
	informer cache.SharedIndexInformer
	resource schema.GroupResource
}

// Informer returns the SharedIndexInformer.
func (f *genericInformer) Informer() cache.SharedIndexInformer {
	return f.informer
}

// Lister returns the GenericLister.
func (f *genericInformer) Lister() cache.GenericLister {
	return cache.NewGenericLister(f.Informer().GetIndexer(), f.resource)
}

// ForResource gives generic access to a shared informer of the matching type
func (f *sharedInformerFactory) ForResource(resource schema.GroupVersionResource) (GenericInformer, error) {
	switch resource {
	// Group=apis, Version=v1alpha1
	case v1alpha1.SchemeGroupVersion.WithResource("backendpolicies"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().BackendPolicies().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("gateways"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().Gateways().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("gatewayclasses"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().GatewayClasses().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("httproutes"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().HTTPRoutes().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("tcproutes"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().TCPRoutes().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("tlsroutes"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().TLSRoutes().Informer()}, nil
	case v1alpha1.SchemeGroupVersion.WithResource("udproutes"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Apis().V1alpha1().UDPRoutes().Informer()}, nil

	}
	return nil, fmt.Errorf("no informer found for %v", resource)
}