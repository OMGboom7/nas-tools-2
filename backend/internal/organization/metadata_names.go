package organization

import "regexp"

var seasonArtworkNamePattern = regexp.MustCompile(`^season[0-9]{2}-(poster|banner|landscape)$`)
