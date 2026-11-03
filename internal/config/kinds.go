package config

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/internal/suggest"
)

// servedKind is a kind as the API server serves it.
type servedKind struct {
	namespaced bool
	// resource is the plural that serves the kind, such as gadgets.
	resource string
	// crd says a CRD under crds serves the kind. schema is that CRD's
	// openAPIV3Schema at the kind's version, or nil.
	crd    bool
	schema map[string]any
}

// servedKinds holds each kind that a CRD under crds or Kubernetes itself
// serves.
type servedKinds map[schema.GroupVersionKind]servedKind

// builtinKinds are the kinds that Kubernetes 1.37 serves without a CRD and
// that a client can watch, by whether they are namespaced.
var builtinKinds = map[bool][]string{
	true: {
		"v1/ConfigMap", "v1/Endpoints", "v1/Event", "v1/LimitRange", "v1/PersistentVolumeClaim", "v1/Pod", "v1/PodTemplate",
		"v1/ReplicationController", "v1/ResourceQuota", "v1/Secret", "v1/Service", "v1/ServiceAccount",
		"apps/v1/ControllerRevision", "apps/v1/DaemonSet", "apps/v1/Deployment", "apps/v1/ReplicaSet", "apps/v1/StatefulSet",
		"autoscaling/v1/HorizontalPodAutoscaler", "autoscaling/v2/HorizontalPodAutoscaler", "batch/v1/CronJob", "batch/v1/Job",
		"certificates.k8s.io/v1/PodCertificateRequest", "coordination.k8s.io/v1/Lease", "discovery.k8s.io/v1/EndpointSlice",
		"events.k8s.io/v1/Event", "networking.k8s.io/v1/Ingress", "networking.k8s.io/v1/NetworkPolicy",
		"policy/v1/PodDisruptionBudget", "rbac.authorization.k8s.io/v1/Role", "rbac.authorization.k8s.io/v1/RoleBinding",
		"resource.k8s.io/v1/ResourceClaim", "resource.k8s.io/v1/ResourceClaimTemplate", "storage.k8s.io/v1/CSIStorageCapacity",
	},
	false: {
		"v1/Namespace", "v1/Node", "v1/PersistentVolume",
		"admissionregistration.k8s.io/v1/MutatingAdmissionPolicy", "admissionregistration.k8s.io/v1/MutatingAdmissionPolicyBinding",
		"admissionregistration.k8s.io/v1/MutatingWebhookConfiguration", "admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy",
		"admissionregistration.k8s.io/v1/ValidatingAdmissionPolicyBinding", "admissionregistration.k8s.io/v1/ValidatingWebhookConfiguration",
		"apiextensions.k8s.io/v1/CustomResourceDefinition", "apiregistration.k8s.io/v1/APIService",
		"certificates.k8s.io/v1/CertificateSigningRequest", "certificates.k8s.io/v1/ClusterTrustBundle",
		"flowcontrol.apiserver.k8s.io/v1/FlowSchema", "flowcontrol.apiserver.k8s.io/v1/PriorityLevelConfiguration",
		"networking.k8s.io/v1/IngressClass", "networking.k8s.io/v1/IPAddress", "networking.k8s.io/v1/ServiceCIDR",
		"node.k8s.io/v1/RuntimeClass", "rbac.authorization.k8s.io/v1/ClusterRole", "rbac.authorization.k8s.io/v1/ClusterRoleBinding",
		"resource.k8s.io/v1/DeviceClass", "resource.k8s.io/v1/DeviceTaintRule", "resource.k8s.io/v1/ResourceSlice",
		"scheduling.k8s.io/v1/PriorityClass", "storage.k8s.io/v1/CSIDriver", "storage.k8s.io/v1/CSINode", "storage.k8s.io/v1/StorageClass",
		"storage.k8s.io/v1/VolumeAttachment", "storage.k8s.io/v1/VolumeAttributesClass", "storagemigration.k8s.io/v1/StorageVersionMigration",
	},
}

// builtins holds builtinKinds. Each one's resource is its kind's plural, as
// Kubernetes names them all.
var builtins = func() servedKinds {
	kinds := servedKinds{}
	for namespaced, spellings := range builtinKinds {
		for _, spelling := range spellings {
			kind, err := parseGVK(spelling)
			if err != nil {
				panic(err)
			}
			plural, _ := meta.UnsafeGuessKindToResource(kind)
			kinds[kind] = servedKind{namespaced: namespaced, resource: plural.Resource}
		}
	}
	return kinds
}()

// readKinds adds to builtins each served version of each
// CustomResourceDefinition in paths. A CRD without a scope is namespaced, as
// Kubernetes defaulted it before apiextensions.k8s.io/v1.
func readKinds(paths []string) (servedKinds, error) {
	documents, err := ReadManifests(paths)
	if err != nil {
		return nil, fmt.Errorf("crds: %w", err)
	}
	kinds := maps.Clone(builtins)
	for _, document := range documents {
		if document["kind"] != "CustomResourceDefinition" {
			continue
		}
		spec, _ := document["spec"].(map[string]any)
		names, _ := spec["names"].(map[string]any)
		group, _ := spec["group"].(string)
		kind, _ := names["kind"].(string)
		plural, _ := names["plural"].(string)
		versions, _ := spec["versions"].([]any)
		for _, v := range versions {
			version, _ := v.(map[string]any)
			if version["served"] != true {
				continue
			}
			name, _ := version["name"].(string)
			validation, _ := version["schema"].(map[string]any)
			root, _ := validation["openAPIV3Schema"].(map[string]any)
			kinds[schema.GroupVersionKind{Group: group, Version: name, Kind: kind}] = servedKind{
				namespaced: spec["scope"] != "Cluster", resource: plural, crd: true, schema: root,
			}
		}
	}
	return kinds, nil
}

// serving finds how the API server serves kind. It refuses a kind that no
// CRD under crds and no built-in kind serves, and lists what the CRDs serve.
// It suggests each served kind whose kind or plural equals kind.Kind,
// ignoring case.
func (k servedKinds) serving(kind schema.GroupVersionKind) (servedKind, error) {
	if served, found := k[kind]; found {
		return served, nil
	}
	var near, fromCRDs []string
	for other, served := range k {
		spelling := other.GroupVersion().String() + "/" + other.Kind
		if strings.EqualFold(other.Kind, kind.Kind) || strings.EqualFold(served.resource, kind.Kind) {
			near = append(near, spelling)
		}
		if served.crd {
			fromCRDs = append(fromCRDs, spelling)
		}
	}
	slices.Sort(near)
	slices.Sort(fromCRDs)
	message := "no CRD under crds and no built-in kind serves it; crds holds no CRD"
	if len(fromCRDs) > 0 {
		message = "no CRD under crds and no built-in kind serves it; the CRDs serve " + suggest.List(fromCRDs, "and")
	}
	if len(near) > 0 {
		message += "; did you mean " + suggest.List(near, "or") + "?"
	}
	return servedKind{}, errors.New(message)
}
