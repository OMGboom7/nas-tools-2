package cloudflare

import (
	"encoding/json"
	"net/netip"
	"strings"
	"unicode"
)

type HostPlan struct {
	BeforeIP string `json:"beforeIP"`
	AfterIP  string `json:"afterIP"`
	Replaced int    `json:"replaced"`
	Input    string `json:"-"`
	Raw      string `json:"-"`
}

func hostLines(v json.RawMessage) ([]string, bool, error) {
	var text string
	if json.Unmarshal(v, &text) == nil {
		return strings.Split(text, "\n"), true, nil
	}
	var lines []string
	if string(v) == "null" || json.Unmarshal(v, &lines) != nil || lines == nil {
		return nil, false, ErrState
	}
	return lines, false, nil
}

func firstAddress(line string) (netip.Addr, int, int, bool) {
	start := strings.IndexFunc(line, func(c rune) bool { return !unicode.IsSpace(c) })
	if start < 0 || line[start] == '#' {
		return netip.Addr{}, 0, 0, false
	}
	end := strings.IndexFunc(line[start:], unicode.IsSpace)
	if end < 0 {
		return netip.Addr{}, 0, 0, false
	}
	end += start
	address, err := netip.ParseAddr(line[start:end])
	return address, start, end, err == nil
}

func PlanHosts(raw string, s Settings, best string) (HostPlan, error) {
	values, err := object(raw)
	if err != nil {
		return HostPlan{}, err
	}
	var enabled bool
	if json.Unmarshal(values["enable"], &enabled) != nil || !enabled {
		return HostPlan{}, ErrState
	}
	lines, textFormat, err := hostLines(values["hosts"])
	if err != nil {
		return HostPlan{}, err
	}
	before, err := netip.ParseAddr(s.CFIP)
	if err != nil {
		return HostPlan{}, ErrState
	}
	after, err := netip.ParseAddr(best)
	if err != nil {
		return HostPlan{}, ErrResult
	}
	if s.Check {
		counts := map[netip.Addr]int{}
		for _, line := range lines {
			if ip, _, _, ok := firstAddress(line); ok {
				counts[ip]++
			}
		}
		maximum, ties := 0, 0
		var chosen netip.Addr
		for ip, count := range counts {
			if count > maximum {
				maximum, ties, chosen = count, 1, ip
			} else if count == maximum {
				ties++
			}
		}
		if ties == 1 {
			before = chosen
		}
	}
	plan := HostPlan{BeforeIP: before.String(), AfterIP: after.String(), Raw: raw}
	for i, line := range lines {
		ip, start, end, valid := firstAddress(line)
		if valid && ip == before {
			plan.Replaced++
			if before != after {
				lines[i] = line[:start] + after.String() + line[end:]
			}
		}
	}
	if plan.Replaced == 0 {
		return HostPlan{}, ErrState
	}
	plan.Input = strings.Join(lines, "\n")
	if before == after {
		return plan, nil
	}
	if textFormat {
		values["hosts"], err = json.Marshal(plan.Input)
	} else {
		values["hosts"], err = json.Marshal(lines)
	}
	if err != nil {
		return HostPlan{}, err
	}
	encoded, err := json.Marshal(values)
	if err != nil || len(encoded) > 64<<10 {
		return HostPlan{}, ErrState
	}
	plan.Raw = string(encoded)
	return plan, nil
}
