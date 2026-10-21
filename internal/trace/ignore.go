package trace

// Ignored reports whether c is on the published list of requests no check
// judges: Events, Leases, TokenReviews, the access-review kinds,
// non-resource URLs and unmapped requests.
func Ignored(c Call) bool {
	if c.Ref.Resource == "" {
		return true
	}
	return c.Ref.System == "kube" && ignoredResources[groupResource{c.Ref.Group, c.Ref.Resource}]
}

type groupResource struct{ group, resource string }

var ignoredResources = map[groupResource]bool{
	{"", "events"}:                                        true,
	{"events.k8s.io", "events"}:                           true,
	{"coordination.k8s.io", "leases"}:                     true,
	{"authentication.k8s.io", "tokenreviews"}:             true,
	{"authorization.k8s.io", "subjectaccessreviews"}:      true,
	{"authorization.k8s.io", "selfsubjectaccessreviews"}:  true,
	{"authorization.k8s.io", "localsubjectaccessreviews"}: true,
	{"authorization.k8s.io", "selfsubjectrulesreviews"}:   true,
}
