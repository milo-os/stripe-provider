// SPDX-License-Identifier: AGPL-3.0-only

// Package v1alpha1 contains API Schema definitions for the
// stripe.billing.miloapis.com v1alpha1 API group.
//
// The schemas in this package are owned by the stripe-provider service.
// Resources of the parent group billing.miloapis.com (PaymentMethod,
// PaymentMethodClass) intentionally have no knowledge of these types.
// +kubebuilder:object:generate=true
// +groupName=stripe.billing.miloapis.com
package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

var (
	// GroupVersion is group version used to register these objects.
	GroupVersion = schema.GroupVersion{Group: "stripe.billing.miloapis.com", Version: "v1alpha1"}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme.
	SchemeBuilder = &schemeBuilder{groupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// schemeBuilder replaces controller-runtime's scheme.Builder, which
// controller-runtime 0.25 deprecates. Same as billing's.
type schemeBuilder struct {
	runtime.SchemeBuilder
	groupVersion schema.GroupVersion
}

func (b *schemeBuilder) Register(objs ...runtime.Object) *schemeBuilder {
	b.SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(b.groupVersion, objs...)
		metav1.AddToGroupVersion(s, b.groupVersion)
		return nil
	})
	return b
}
