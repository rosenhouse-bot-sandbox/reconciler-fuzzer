package plan

// Upgrade is what a restart changes about the controller. Load resolves
// Binary and CRDs against the plan's directory. A nil Args keeps the
// launch's args, and an empty one drops them.
type Upgrade struct {
	Binary string   `json:"binary,omitempty"`
	Args   []string `json:"args,omitzero"`
	CRDs   []string `json:"crds,omitempty"`
}
