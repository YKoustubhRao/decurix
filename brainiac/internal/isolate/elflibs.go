package isolate

import (
	"debug/elf"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// A program from a release build (llama.cpp's) links shared libraries that
// sit in its own build, found through a search path of "$ORIGIN", the
// program's own directory. A sandbox that grants the program must grant
// those libraries too, and no more: they are read from the ELF file itself,
// the way the dynamic loader finds them.

// maxObjects bounds the walk; a real build links a few dozen.
const maxObjects = 512

// elfObject is what the loader reads from one ELF file: the libraries it
// names and where it says to look for them.
type elfObject struct {
	needed, rpath, runpath []string
}

// readELF reads an ELF file's DT_NEEDED, DT_RPATH and DT_RUNPATH. A file
// with no dynamic section (a static program) names nothing.
func readELF(path string) (elfObject, error) {
	f, err := elf.Open(path)
	if err != nil {
		return elfObject{}, err
	}
	defer f.Close()
	var o elfObject
	if o.needed, err = f.DynString(elf.DT_NEEDED); err != nil {
		return o, err
	}
	for _, t := range []struct {
		tag elf.DynTag
		dst *[]string
	}{{elf.DT_RPATH, &o.rpath}, {elf.DT_RUNPATH, &o.runpath}} {
		v, err := f.DynString(t.tag)
		if err != nil {
			return o, err
		}
		for _, s := range v {
			for _, d := range strings.Split(s, ":") { // the ELF's own separator, on any system
				if d != "" {
					*t.dst = append(*t.dst, d)
				}
			}
		}
	}
	return o, nil
}

// searchDir is one directory the loader looks in, and whether the object
// named it through $ORIGIN: its own build.
type searchDir struct {
	path   string
	origin bool
}

// expandDirs is dirs with $ORIGIN replaced by origin. An entry with another
// substitution ($LIB, $PLATFORM) or a relative one is left out: what it
// means depends on the machine or the working directory, not the build.
func expandDirs(dirs []string, origin string) []searchDir {
	var out []searchDir
	for _, d := range dirs {
		e := strings.NewReplacer("${ORIGIN}", origin, "$ORIGIN", origin).Replace(d)
		if strings.Contains(e, "$") || !filepath.IsAbs(e) {
			continue
		}
		out = append(out, searchDir{path: filepath.Clean(e), origin: e != d})
	}
	return out
}

// within says p is beneath root.
func within(root, p string) bool {
	rel, err := filepath.Rel(root, p)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}

// ownLibs is what a program loads from its own build.
type ownLibs struct {
	// files are the libraries, by the path the loader opens (a symlink kept
	// as one) and by their real path.
	files []string
	// origin is the program's own $ORIGIN search directories, expanded,
	// inside its build.
	origin []string
	// outside are libraries the build's own $ORIGIN path finds outside the
	// build: not granted, so a program that needs them will not load.
	outside []string
}

// ownLibraries finds the shared libraries a program loads from its own
// build: those its ELF names (DT_NEEDED), looked up as the dynamic loader
// does (DT_RPATH of the object and of what loaded it, unless it has
// DT_RUNPATH, then DT_RUNPATH), and theirs in turn. The build is the
// directory of the real program; a library found outside it is the
// system's, which the policy's system paths already hold, and is not walked.
// plugins are libraries the program opens itself at run time (a ggml
// backend), which no DT_NEEDED names; their own libraries are walked the
// same way. It fails when the program, or a library in its build, is not an
// ELF file it can read.
func ownLibraries(program string, plugins []string) (ownLibs, error) {
	real, err := filepath.EvalSymlinks(program)
	if err != nil {
		return ownLibs{}, err
	}
	root := filepath.Dir(real)
	exe, err := readELF(real)
	if err != nil {
		return ownLibs{}, err
	}
	type item struct {
		path      string
		o         elfObject
		inherited []searchDir // the DT_RPATH of what loaded it, up to the program
	}
	rpathOf := func(o elfObject, origin string) []searchDir {
		if len(o.runpath) > 0 {
			return nil // the loader ignores DT_RPATH beside DT_RUNPATH
		}
		return expandDirs(o.rpath, origin)
	}
	var out ownLibs
	for _, d := range append(rpathOf(exe, root), expandDirs(exe.runpath, root)...) {
		if d.origin && (d.path == root || within(root, d.path)) {
			out.origin = append(out.origin, d.path)
		}
	}
	exeRpath := rpathOf(exe, root)
	queue := []item{{path: real, o: exe}}
	for _, p := range plugins {
		o, err := readELF(p)
		if err != nil {
			return out, fmt.Errorf("%s: %w", p, err)
		}
		queue = append(queue, item{path: p, o: o, inherited: exeRpath})
	}
	granted, found := map[string]bool{}, map[string]bool{}
	grant := func(p string) {
		if !granted[p] {
			granted[p] = true
			out.files = append(out.files, p)
		}
	}
	for n := 0; len(queue) > 0; n++ {
		if n >= maxObjects {
			return out, errors.New("more libraries than any build links")
		}
		it := queue[0]
		queue = queue[1:]
		origin := filepath.Dir(it.path)
		own := rpathOf(it.o, origin)
		dirs := append(append([]searchDir{}, own...), it.inherited...)
		if len(it.o.runpath) > 0 {
			dirs = expandDirs(it.o.runpath, origin)
		}
		children := append(append([]searchDir{}, own...), it.inherited...)
		for _, name := range it.o.needed {
			if found[name] {
				continue // the loader loads a name once
			}
			var cands []searchDir
			if strings.Contains(name, "/") {
				if filepath.IsAbs(name) {
					cands = []searchDir{{path: filepath.Dir(name)}}
					name = filepath.Base(name)
				}
			} else {
				cands = dirs
			}
			for _, d := range cands {
				cand := filepath.Join(d.path, name)
				if fi, err := os.Stat(cand); err != nil || fi.IsDir() {
					continue
				}
				found[name] = true
				r, err := filepath.EvalSymlinks(cand)
				if err != nil || !within(root, cand) || !within(root, r) {
					if d.origin && !systemPath(r) {
						out.outside = append(out.outside, cand)
					}
					break
				}
				o, err := readELF(r)
				if err != nil {
					return out, fmt.Errorf("%s: %w", cand, err)
				}
				grant(cand)
				grant(r)
				queue = append(queue, item{path: cand, o: o, inherited: children})
				break
			}
		}
	}
	return out, nil
}

// systemPath says p is beneath a path every policy already reads.
func systemPath(p string) bool {
	for _, s := range systemRead {
		if p == s || within(s, p) {
			return true
		}
	}
	return false
}
