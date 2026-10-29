// Package slicex holds small slice helpers shared by modules.
package slicex

// Ptrs returns pointers to the elements of xs, for helpers that fill them in place.
func Ptrs[T any](xs []T) []*T {
	out := make([]*T, len(xs))
	for i := range xs {
		out[i] = &xs[i]
	}
	return out
}
