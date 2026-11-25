//go:build !unix

package sandbox

// alive reports true so containers are never removed on a guess.
func alive(int) bool { return true }
