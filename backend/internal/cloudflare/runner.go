package cloudflare

import (
	"context"
	"encoding/csv"
	"errors"
	"io"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type Result struct {
	BestIP           string `json:"bestIP"`
	Version          string `json:"version"`
	ExecutableDigest string `json:"executableDigest"`
	InputDigest      string `json:"inputDigest"`
	Rows             int    `json:"rows"`
}

func ipRanges(raw string, s Settings) ([]netip.Prefix, []string, error) {
	if len(raw) > 4<<20 {
		return nil, nil, ErrState
	}
	prefixes := []netip.Prefix{}
	lines := []string{}
	seen := map[netip.Prefix]bool{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		if line == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(line)
		if err != nil {
			address, e := netip.ParseAddr(line)
			if e != nil {
				return nil, nil, ErrState
			}
			prefix = netip.PrefixFrom(address, address.BitLen())
		}
		if prefix.Addr().Zone() != "" {
			return nil, nil, ErrState
		}
		if prefix.Addr().Is4() && !s.IPv4 || prefix.Addr().Is6() && !s.IPv6 {
			continue
		}
		prefix = prefix.Masked()
		if !seen[prefix] {
			seen[prefix] = true
			prefixes = append(prefixes, prefix)
			lines = append(lines, prefix.String())
		}
		if len(prefixes) > 65536 {
			return nil, nil, ErrState
		}
	}
	if len(prefixes) == 0 {
		return nil, nil, ErrState
	}
	return prefixes, lines, nil
}

func runInput(in Installation, s Settings, args []string) ([]byte, []netip.Prefix, error) {
	var raw strings.Builder
	for _, file := range []struct {
		wanted bool
		name   string
	}{{s.IPv4, "ip.txt"}, {s.IPv6, "ipv6.txt"}} {
		if !file.wanted {
			continue
		}
		data, err := regular(filepath.Join(in.Root, in.Directory, file.name), 2<<20)
		if err != nil {
			return nil, nil, err
		}
		if _, _, err := ipRanges(string(data), Settings{IPv4: file.name == "ip.txt", IPv6: file.name == "ipv6.txt"}); err != nil {
			return nil, nil, err
		}
		raw.Write(data)
		raw.WriteByte('\n')
	}
	inputRanges, lines, err := ipRanges(raw.String(), s)
	if err != nil {
		return nil, nil, err
	}
	// The original advanced -ip option overrides the input file. Bind result
	// verification to those explicit targets when it is present as well.
	explicit := ""
	for n, arg := range args {
		if arg == "-ip" || arg == "--ip" {
			if n+1 >= len(args) {
				return nil, nil, ErrState
			}
			explicit = args[n+1]
		}
		if strings.HasPrefix(arg, "-ip=") {
			explicit = strings.TrimPrefix(arg, "-ip=")
		}
		if strings.HasPrefix(arg, "--ip=") {
			explicit = strings.TrimPrefix(arg, "--ip=")
		}
	}
	if explicit != "" {
		inputRanges, _, err = ipRanges(strings.ReplaceAll(explicit, ",", "\n"), s)
		if err != nil {
			return nil, nil, err
		}
	}
	return []byte(strings.Join(lines, "\n") + "\n"), inputRanges, nil
}

func csvResult(data []byte, ranges []netip.Prefix) (string, int, error) {
	if len(data) == 0 || len(data) > 4<<20 {
		return "", 0, ErrResult
	}
	reader := csv.NewReader(strings.NewReader(strings.TrimPrefix(string(data), "\ufeff")))
	header, err := reader.Read()
	if err != nil || len(header) < 2 {
		return "", 0, ErrResult
	}
	key := strings.ToLower(strings.Join(strings.Fields(header[0]), ""))
	if key != "ip地址" && key != "ipaddress" && key != "ip" {
		return "", 0, ErrResult
	}
	best, rows := "", 0
	for {
		row, err := reader.Read()
		if err == io.EOF {
			break
		}
		if err != nil || len(row) != len(header) {
			return "", 0, ErrResult
		}
		address, err := netip.ParseAddr(strings.TrimSpace(row[0]))
		if err != nil || address.Zone() != "" {
			return "", 0, ErrResult
		}
		known := false
		for _, p := range ranges {
			if p.Contains(address) {
				known = true
				break
			}
		}
		if !known {
			return "", 0, ErrResult
		}
		rows++
		if rows > 65536 {
			return "", 0, ErrResult
		}
		if best == "" {
			best = address.String()
		}
	}
	if best == "" {
		return "", 0, ErrResult
	}
	return best, rows, nil
}

// Run only executes the approved native program. Each invocation owns a fresh
// input and output directory, so a prior result_hosts.txt cannot prove success.
func Run(ctx context.Context, in Installation, s Settings, before Before) (Result, error) {
	if !s.IPv4 && !s.IPv6 {
		s.IPv4 = true
	}
	if err := validateInstall(in, s, runtime.GOOS, runtime.GOARCH); err != nil {
		return Result{}, err
	}
	args, err := Arguments(s.AdditionalArgs)
	if err != nil {
		return Result{}, err
	}
	input, ranges, err := runInput(in, s, args)
	if err != nil {
		return Result{}, err
	}
	digest := sum(input)
	if err := permit(ctx, before, Operation{Kind: "prepare", Target: in.Version, Digest: sumJSON([]any{in.Digest, digest, args})}); err != nil {
		return Result{}, err
	}
	parent := filepath.Join(in.Root, ".go-runs")
	if err := plainDirectory(parent); err != nil {
		return Result{}, err
	}
	directory, err := os.MkdirTemp(parent, "run-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(directory)
	inputPath, outputPath := filepath.Join(directory, "input.txt"), filepath.Join(directory, "result.csv")
	if err := writeNew(inputPath, input, 0600); err != nil {
		return Result{}, err
	}
	args = append(args, "-f", inputPath, "-o", outputPath)
	if err := permit(ctx, before, Operation{Kind: "execute", Target: in.Version, Digest: sumJSON([]any{in.Digest, digest, s.AdditionalArgs})}); err != nil {
		return Result{}, err
	}
	// Recheck the installation after the durable intent and before execution.
	if err := validateInstall(in, s, runtime.GOOS, runtime.GOARCH); err != nil {
		return Result{}, err
	}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(runCtx, filepath.Join(in.Root, in.Directory, in.Binary), args...)
	command.Dir = directory
	command.Env = []string{}
	for _, key := range []string{"PATH", "TMPDIR", "SSL_CERT_FILE", "SSL_CERT_DIR", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		if value, ok := os.LookupEnv(key); ok {
			command.Env = append(command.Env, key+"="+value)
		}
	}
	command.Stdout = io.Discard
	command.Stderr = io.Discard
	command.WaitDelay = 2 * time.Second
	if err := command.Run(); err != nil {
		return Result{}, errors.Join(ErrResult, err)
	}
	if err := runCtx.Err(); err != nil {
		return Result{}, errors.Join(ErrResult, err)
	}
	data, err := regular(outputPath, 4<<20)
	if err != nil {
		return Result{}, ErrResult
	}
	best, rows, err := csvResult(data, ranges)
	if err != nil {
		return Result{}, err
	}
	return Result{BestIP: best, Version: in.Version, ExecutableDigest: in.Digest, InputDigest: digest, Rows: rows}, nil
}
