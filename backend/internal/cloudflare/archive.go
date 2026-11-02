package cloudflare

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"debug/elf"
	"debug/macho"
	"io"
	"path"
	"strings"
)

func nativeExecutable(data []byte, osName, arch string) error {
	if len(data) == 0 || len(data) > 64<<20 {
		return ErrState
	}
	if osName == "linux" {
		file, err := elf.NewFile(bytes.NewReader(data))
		if err != nil {
			return ErrState
		}
		defer file.Close()
		expected := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64, "386": elf.EM_386, "arm": elf.EM_ARM}[arch]
		if expected == 0 || file.Machine != expected || file.Type != elf.ET_EXEC && file.Type != elf.ET_DYN {
			return ErrState
		}
		return nil
	}
	if osName == "darwin" {
		file, err := macho.NewFile(bytes.NewReader(data))
		if err != nil {
			return ErrState
		}
		defer file.Close()
		expected := map[string]macho.Cpu{"amd64": macho.CpuAmd64, "arm64": macho.CpuArm64}[arch]
		if expected == 0 || file.Cpu != expected || file.Type != macho.TypeExec {
			return ErrState
		}
		return nil
	}
	return ErrState
}

func unpack(name string, data []byte, osName, arch string) (map[string][]byte, error) {
	files := map[string][]byte{}
	count, total := 0, int64(0)
	add := func(name string, size int64, mode byte, reader io.Reader) error {
		count++
		total += size
		name = strings.ReplaceAll(name, "\\", "/")
		clean := path.Clean(name)
		if count > 128 || size < 0 || size > 64<<20 || total > 96<<20 || path.IsAbs(name) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(clean, ":") {
			return ErrState
		}
		if mode == 'd' {
			return nil
		}
		if mode != 'f' {
			return ErrState
		}
		base := path.Base(clean)
		if base != "CloudflareST" && base != "cfst" && base != "ip.txt" && base != "ipv6.txt" {
			return nil
		}
		if files[base] != nil || base == "ip.txt" && size > 2<<20 || base == "ipv6.txt" && size > 2<<20 {
			return ErrState
		}
		contents, err := io.ReadAll(io.LimitReader(reader, size+1))
		if err != nil || int64(len(contents)) != size {
			return ErrState
		}
		files[base] = contents
		return nil
	}
	if strings.HasSuffix(name, ".zip") {
		archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
		if err != nil {
			return nil, ErrResult
		}
		for _, f := range archive.File {
			mode := byte('f')
			if f.FileInfo().IsDir() {
				mode = 'd'
			} else if !f.Mode().IsRegular() {
				return nil, ErrState
			}
			if f.UncompressedSize64 > 64<<20 {
				return nil, ErrState
			}
			r, err := f.Open()
			if err != nil {
				return nil, err
			}
			err = add(f.Name, int64(f.UncompressedSize64), mode, r)
			r.Close()
			if err != nil {
				return nil, err
			}
		}
	} else if strings.HasSuffix(name, ".tar.gz") {
		compressed, err := gzip.NewReader(bytes.NewReader(data))
		if err != nil {
			return nil, ErrResult
		}
		defer compressed.Close()
		archive := tar.NewReader(compressed)
		for {
			f, err := archive.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, ErrResult
			}
			mode := byte('f')
			if f.Typeflag == tar.TypeDir {
				mode = 'd'
			} else if f.Typeflag != tar.TypeReg && f.Typeflag != tar.TypeRegA {
				return nil, ErrState
			}
			if err := add(f.Name, f.Size, mode, archive); err != nil {
				return nil, err
			}
		}
	} else {
		return nil, ErrState
	}
	if files["CloudflareST"] != nil && files["cfst"] != nil {
		return nil, ErrState
	}
	binary := files["CloudflareST"]
	if binary == nil {
		binary = files["cfst"]
	}
	if nativeExecutable(binary, osName, arch) != nil {
		return nil, ErrState
	}
	return files, nil
}
