package kube

import (
	"context"
	"fmt"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
)

const serviceAccount = "reconciler-fuzzer-controller"

// tokenLifetime outlasts any run, so a controller request never fails with
// an expired token.
const tokenLifetime = 7 * 24 * time.Hour

// createIdentity creates the controller's ServiceAccount, binds its roles and
// returns a config that authenticates as it.
func createIdentity(ctx context.Context, client kubernetes.Interface, admin *rest.Config, namespace string, roles []rbacv1.Role, clusterRoles []rbacv1.ClusterRole) (*rest.Config, error) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Name: serviceAccount}}
	if _, err := client.CoreV1().ServiceAccounts(namespace).Create(ctx, sa, metav1.CreateOptions{}); err != nil {
		return nil, fmt.Errorf("creating ServiceAccount %s: %w", serviceAccount, err)
	}
	subjects := []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: serviceAccount, Namespace: namespace}}
	for _, role := range roles {
		role.Namespace = namespace
		if _, err := client.RbacV1().Roles(namespace).Create(ctx, &role, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating Role %s: %w", role.Name, err)
		}
		binding := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: role.Name},
			Subjects:   subjects,
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		}
		if _, err := client.RbacV1().RoleBindings(namespace).Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating RoleBinding %s: %w", role.Name, err)
		}
	}
	for _, clusterRole := range clusterRoles {
		clusterRole.Name += "-" + namespace
		if _, err := client.RbacV1().ClusterRoles().Create(ctx, &clusterRole, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating ClusterRole %s: %w", clusterRole.Name, err)
		}
		binding := &rbacv1.ClusterRoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: clusterRole.Name},
			Subjects:   subjects,
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: clusterRole.Name},
		}
		if _, err := client.RbacV1().ClusterRoleBindings().Create(ctx, binding, metav1.CreateOptions{}); err != nil {
			return nil, fmt.Errorf("creating ClusterRoleBinding %s: %w", clusterRole.Name, err)
		}
	}
	seconds := int64(tokenLifetime.Seconds())
	request := &authenticationv1.TokenRequest{Spec: authenticationv1.TokenRequestSpec{ExpirationSeconds: &seconds}}
	token, err := client.CoreV1().ServiceAccounts(namespace).CreateToken(ctx, serviceAccount, request, metav1.CreateOptions{})
	if err != nil {
		return nil, fmt.Errorf("requesting a token for ServiceAccount %s: %w", serviceAccount, err)
	}
	config := rest.AnonymousClientConfig(admin)
	config.BearerToken = token.Status.Token
	return config, nil
}
