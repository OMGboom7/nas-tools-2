package mediameta

import (
	"errors"
	"regexp"
	"strings"
)

// A marker must follow release context; e.g. "The Godfather Part II 1974"
// retains its film title. Token boundaries prevent CD123/discfan substring cuts.
var releasePartToken = regexp.MustCompile(`(?i)(?:^|[ ._\[(-])((?:PART|CD|DVD|DISK|DISC)(?:[ ._\[(-]*(?:[0-9]{1,2}|III|II|I|[ABC]))?)`)

var manualPartToken = regexp.MustCompile(`^(?:PART(?:[0-9ABI]{0,2}|C|III)|(?:CD|DVD|DISK|DISC)[0-9]{0,2})$`)

// An empty explicit part clears automatic recognition; a nil selection retains it.
func NormalizePart(value string) (string, error) {
	if len(value) > 32 {
		return "", errors.New("invalid manual release part")
	}
	value = strings.ToUpper(strings.TrimSpace(value))
	if value != "" && !manualPartToken.MatchString(value) {
		return "", errors.New("invalid manual release part")
	}
	return value, nil
}

func partReleaseContext(input string, before int) bool {
	end := partContextEnd(input)
	return end >= 0 && end <= before
}
func partContextEnd(input string) int {
	first := -1
	for _, pattern := range []*regexp.Regexp{yearToken, resolutionToken, sourceToken, nameSeasonToken} {
		for _, span := range pattern.FindAllStringIndex(input, -1) {
			if pattern == yearToken && span[1] < len(input) && !strings.ContainsRune(" ._])(", rune(input[span[1]])) {
				continue
			}
			prefix := leadingGroup.ReplaceAllString(input[:span[0]], "")
			if strings.Trim(prefix, " ._[]()-") == "" {
				continue
			}
			if first < 0 || span[1] < first {
				first = span[1]
			}
		}
	}
	return first
}
func releasePart(input string, inheritedContext bool) (string, string, error) {
	return scanReleaseParts(input, inheritedContext, false)
}

func scanReleaseParts(input string, inheritedContext, explicitPart bool) (string, string, error) {
	masked := []byte(input)
	part := ""
	contextEnd := partContextEnd(input)
	for _, span := range releasePartToken.FindAllStringSubmatchIndex(input, -1) {
		start, end := span[2], span[3]
		if end < len(input) && !strings.ContainsRune(" ._[]()-", rune(input[end])) {
			continue
		}
		if !inheritedContext && (contextEnd < 0 || contextEnd > start) {
			continue
		}
		value := strings.ToUpper(strings.NewReplacer(" ", "", ".", "", "_", "", "-", "", "[", "", "(", "").Replace(input[start:end]))
		if value == "PART" {
			continue
		}
		if part != "" && part != value && !explicitPart {
			return "", input, errors.New("ambiguous release part")
		}
		part = value
		for i := start; i < end; i++ {
			masked[i] = ' '
		}
	}
	return part, string(masked), nil
}
