//go:build !unix

package control

func setUmask(m int) int { return m }
