package kube

import (
	"context"
	"testing"

	authenticationv1 "k8s.io/api/authentication/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/rest"
	k8stesting "k8s.io/client-go/testing"
)

func TestCreateIdentityBindsARoleInItsNamespace(t *testing.T) {
	client := fake.NewClientset()
	client.PrependReactor("create", "serviceaccounts", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if action.GetSubresource() != "token" {
			return false, nil, nil
		}
		return true, &authenticationv1.TokenRequest{Status: authenticationv1.TokenRequestStatus{Token: "t"}}, nil
	})
	roles := []rbacv1.Role{
		{ObjectMeta: metav1.ObjectMeta{Name: "here"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "there", Namespace: "other"}},
	}
	if _, err := createIdentity(context.Background(), client, &rest.Config{}, "run", roles, nil); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, namespace := range map[string]string{"here": "run", "there": "other"} {
		if _, err := client.RbacV1().Roles(namespace).Get(ctx, name, metav1.GetOptions{}); err != nil {
			t.Errorf("Role %s in %s: %v", name, namespace, err)
		}
		binding, err := client.RbacV1().RoleBindings(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Errorf("RoleBinding %s in %s: %v", name, namespace, err)
			continue
		}
		if s := binding.Subjects[0]; s.Name != serviceAccount || s.Namespace != "run" {
			t.Errorf("RoleBinding %s binds %+v, want the ServiceAccount in run.", name, s)
		}
	}
}
