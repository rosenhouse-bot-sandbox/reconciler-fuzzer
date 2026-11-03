// Package suggest finds the name a mistyped one most likely meant, and
// lists the valid names.
package suggest

import "strings"

// Nearest is the first of names closest to key, ignoring case, within two
// edits and fewer edits than key has letters. It is empty when none is.
func Nearest(key string, names []string) string {
	best, bestDistance := "", 3
	for _, name := range names {
		if distance := editDistance(strings.ToLower(key), strings.ToLower(name)); distance < bestDistance && distance < len(key) {
			best, bestDistance = name, distance
		}
	}
	return best
}

// List joins names as "a, b or c", with conjunction in place of "or".
func List[Name ~string](names []Name, conjunction string) string {
	var list string
	for i, name := range names {
		switch {
		case i == 0:
		case i == len(names)-1:
			list += " " + conjunction + " "
		default:
			list += ", "
		}
		list += string(name)
	}
	return list
}

// editDistance counts the fewest insertions, deletions, substitutions and
// swaps of adjacent letters that turn a into b.
func editDistance(a, b string) int {
	d := make([][]int, len(a)+1)
	for i := range d {
		d[i] = make([]int, len(b)+1)
		d[i][0] = i
	}
	for j := range d[0] {
		d[0][j] = j
	}
	for i := 1; i <= len(a); i++ {
		for j := 1; j <= len(b); j++ {
			substitution := d[i-1][j-1]
			if a[i-1] != b[j-1] {
				substitution++
			}
			d[i][j] = min(d[i-1][j]+1, d[i][j-1]+1, substitution)
			if i > 1 && j > 1 && a[i-1] == b[j-2] && a[i-2] == b[j-1] {
				d[i][j] = min(d[i][j], d[i-2][j-2]+1)
			}
		}
	}
	return d[len(a)][len(b)]
}
