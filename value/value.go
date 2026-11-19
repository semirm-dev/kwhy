// Package value traces fields of rendered Kubernetes config back to the values line that sets them.
package value

// Kind says how a rendered field relates to the values that produce it.
// The strings are part of the kwhy.value/v1 JSON contract.
type Kind string

const (
	Direct     Kind = "direct"     // the field equals one values leaf
	Composed   Kind = "composed"   // leaves plus template text
	Derived    Kind = "derived"    // template logic computes it from leaves
	Encoded    Kind = "encoded"    // a leaf passed through b64enc
	Hashed     Kind = "hashed"     // checksum of other content
	Chart      Kind = "chart"      // constant in the chart
	Structural Kind = "structural" // a leaf decides whether the field exists
	Random     Kind = "random"     // differs between two identical renders
)
