// Package diskusage reads capacity and identifies the filesystem containing a library path.
package diskusage

type Space struct {
	Volume         string
	TotalBytes     float64
	AvailableBytes float64
}
