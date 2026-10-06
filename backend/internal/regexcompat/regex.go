package regexcompat

import (
	"strings"
	"time"

	"github.com/dlclark/regexp2"
)

func Compile(pattern string, options regexp2.RegexOptions) (*regexp2.Regexp, error) {
	compiled, err := regexp2.Compile(Pattern(pattern), options)
	if err == nil {
		compiled.MatchTimeout = 100 * time.Millisecond
	}
	return compiled, err
}

// Pattern converts Python named groups, named references, and strict end anchors.
func Pattern(pattern string) string {
	var output strings.Builder
	inClass := false
	for index := 0; index < len(pattern); {
		if pattern[index] == '\\' && index+1 < len(pattern) {
			if pattern[index+1] == 'Z' && !inClass {
				output.WriteString(`\z`)
			} else {
				output.WriteString(pattern[index : index+2])
			}
			index += 2
			continue
		}
		if pattern[index] == '[' {
			inClass = true
		} else if pattern[index] == ']' {
			inClass = false
		}
		if !inClass && strings.HasPrefix(pattern[index:], "(?P<") {
			output.WriteString("(?<")
			index += 4
			continue
		}
		if !inClass && strings.HasPrefix(pattern[index:], "(?P=") {
			if end := strings.IndexByte(pattern[index+4:], ')'); end >= 0 {
				output.WriteString(`\k<`)
				output.WriteString(pattern[index+4 : index+4+end])
				output.WriteByte('>')
				index += 5 + end
				continue
			}
		}
		output.WriteByte(pattern[index])
		index++
	}
	return output.String()
}
