package plan

// Upgrade is what a restart changes about the controller. Load resolves
// Binary and CRDs against the plan's directory. A nil Args keeps the
// current args, and an empty one drops them.
type Upgrade struct {
	Binary string   `json:"binary,omitempty"`
	Args   []string `json:"args,omitzero"`
	CRDs   []string `json:"crds,omitempty"`
}

// Upgrades reports whether u sets a binary, args or crds. Empty args or crds
// count as set.
func (u Upgrade) Upgrades() bool { return u.Binary != "" || u.Args != nil || u.CRDs != nil }
