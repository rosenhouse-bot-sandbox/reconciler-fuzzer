package kube

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"sigs.k8s.io/yaml"

	"github.com/rosenhouse-bot-sandbox/reconciler-fuzzer/v2/trace"
)

func newNamespaceName() string {
	b := make([]byte, 4)
	rand.Read(b)
	return "rf-" + hex.EncodeToString(b)
}

// readRBAC decodes every document of paths strictly as a Role or a
// ClusterRole.
func readRBAC(paths []string) ([]rbacv1.Role, []rbacv1.ClusterRole, error) {
	var roles []rbacv1.Role
	var clusterRoles []rbacv1.ClusterRole
	for _, path := range paths {
		documents, err := readDocuments(path)
		if err != nil {
			return nil, nil, err
		}
		for _, document := range documents {
			var header struct {
				APIVersion string `json:"apiVersion"`
				Kind       string `json:"kind"`
			}
			if err := yaml.Unmarshal(document, &header); err != nil {
				return nil, nil, fmt.Errorf("%s: %w", path, err)
			}
			kind := header.Kind
			if header.APIVersion != rbacv1.SchemeGroupVersion.String() {
				kind = header.APIVersion + " " + header.Kind
			}
			switch kind {
			case "Role":
				var role rbacv1.Role
				if err := yaml.UnmarshalStrict(document, &role); err != nil {
					return nil, nil, fmt.Errorf("%s: %w", path, err)
				}
				if role.Namespace != "" {
					return nil, nil, fmt.Errorf("%s: the Role %s sets metadata.namespace, but roles bind in the run namespace", path, role.Name)
				}
				roles = append(roles, role)
			case "ClusterRole":
				var clusterRole rbacv1.ClusterRole
				if err := yaml.UnmarshalStrict(document, &clusterRole); err != nil {
					return nil, nil, fmt.Errorf("%s: %w", path, err)
				}
				if clusterRole.AggregationRule != nil {
					return nil, nil, fmt.Errorf("%s: the ClusterRole %s sets aggregationRule, which a copy cannot keep", path, clusterRole.Name)
				}
				clusterRoles = append(clusterRoles, clusterRole)
			default:
				return nil, nil, fmt.Errorf("%s: %s %s is not a Role or ClusterRole of %s", path, header.APIVersion, header.Kind, rbacv1.SchemeGroupVersion)
			}
		}
	}
	return roles, clusterRoles, nil
}

// readObjects decodes every document of paths as an object.
func readObjects(paths []string) ([]*unstructured.Unstructured, error) {
	var objects []*unstructured.Unstructured
	for _, path := range paths {
		documents, err := readDocuments(path)
		if err != nil {
			return nil, err
		}
		for _, document := range documents {
			object := &unstructured.Unstructured{}
			if err := yaml.Unmarshal(document, &object.Object); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			if object.GetAPIVersion() == "" || object.GetKind() == "" {
				return nil, fmt.Errorf("%s: an object has no apiVersion or kind", path)
			}
			objects = append(objects, object)
		}
	}
	return objects, nil
}

// readDocuments splits a YAML file into its documents, skipping empty ones.
func readDocuments(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var documents [][]byte
	reader := utilyaml.NewYAMLReader(bufio.NewReader(bytes.NewReader(data)))
	for {
		document, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return documents, nil
		}
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		asJSON, err := yaml.YAMLToJSON(document)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if string(asJSON) != "null" {
			documents = append(documents, document)
		}
	}
}

// ruleKinds lists the kinds the rules name, without wildcards, subresources
// and ignored kinds.
func ruleKinds(roles []rbacv1.Role, clusterRoles []rbacv1.ClusterRole) []schema.GroupResource {
	var rules []rbacv1.PolicyRule
	for _, role := range roles {
		rules = append(rules, role.Rules...)
	}
	for _, role := range clusterRoles {
		rules = append(rules, role.Rules...)
	}
	var kinds []schema.GroupResource
	for _, rule := range rules {
		for _, group := range rule.APIGroups {
			for _, resource := range rule.Resources {
				gr := schema.GroupResource{Group: group, Resource: resource}
				ignored := trace.Ignored(trace.Call{Ref: trace.Ref{Group: group, Resource: resource}})
				if group == "*" || resource == "*" || strings.Contains(resource, "/") || ignored || slices.Contains(kinds, gr) {
					continue
				}
				kinds = append(kinds, gr)
			}
		}
	}
	return kinds
}
