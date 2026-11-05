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

// errNotShared is a library whose ELF header is not ET_DYN: the loader
// cannot load it, so it is not granted.
var errNotShared = errors.New("not an ELF shared object (ET_DYN)")

// openELF parses an ELF file (tests replace it).
var openELF = elf.Open

// readELF reads an ELF file's DT_NEEDED, DT_RPATH and DT_RUNPATH. A file
// with no dynamic section (a static program) names nothing.
func readELF(path string) (elfObject, error) { return readELFOf(path, false) }

// readLib is readELF for a library, whose ELF header must say ET_DYN. That
// is a sanity filter on the header, not proof of a library: a
// position-independent executable is ET_DYN too, and so is any file that
// starts with such a header. What makes a grant safe is where the file is
// and that an ELF names it (ownLibraries), not its contents.
func readLib(path string) (elfObject, error) { return readELFOf(path, true) }

func readELFOf(path string, shared bool) (elfObject, error) {
	f, err := openELF(path)
	if err != nil {
		return elfObject{}, err
	}
	defer f.Close()
	if shared && f.Type != elf.ET_DYN {
		return elfObject{}, errNotShared
	}
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
		e, expanded := expandOrigin(d, origin)
		if strings.Contains(e, "$") || !filepath.IsAbs(e) {
			continue
		}
		out = append(out, searchDir{path: filepath.Clean(e), origin: expanded})
	}
	return out
}

// expandOrigin replaces $ORIGIN in d as glibc does: "${ORIGIN}" anywhere,
// and "$ORIGIN" only where a "/" or the end follows it ("$ORIGINx" is not
// a substitution, and is left as it is).
func expandOrigin(d, origin string) (string, bool) {
	var b strings.Builder
	expanded := false
	for i := 0; i < len(d); {
		if strings.HasPrefix(d[i:], "${ORIGIN}") {
			b.WriteString(origin)
			i += len("${ORIGIN}")
			expanded = true
			continue
		}
		if j := i + len("$ORIGIN"); strings.HasPrefix(d[i:], "$ORIGIN") && (j == len(d) || d[j] == '/') {
			b.WriteString(origin)
			i = j
			expanded = true
			continue
		}
		b.WriteByte(d[i])
		i++
	}
	return b.String(), expanded
}

// buildRoot is where a program's own libraries may resolve: the program's
// directory, or, when that is a bin directory (an install tree with its
// libraries in ../lib), its parent. Not a filesystem root, /usr or
// /usr/local, which every policy already reads. The root is a bound on
// which exact files may be granted, never itself a grant, and the guards
// are not the boundary: ~/bin gives $HOME, and that is safe only because
// nothing but the files an ELF names, reached by its search path, is ever
// granted from it. Never grant the root.
func buildRoot(real string) string {
	dir := filepath.Dir(real)
	if filepath.Base(dir) != "bin" {
		return dir
	}
	parent := filepath.Dir(dir)
	switch filepath.ToSlash(parent) {
	case "/usr", "/usr/local":
		return dir
	}
	if filepath.Dir(parent) == parent {
		return dir
	}
	return parent
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
	// build, and plugins outside it: not granted, so a program that needs
	// them will not load.
	outside []string
	// skipped are files in the build the loader was sent to, and plugins,
	// whose ELF header is not ET_DYN: not granted.
	skipped []string
}

// ownLibraries finds the shared libraries a program loads from its own
// build: those its ELF names (DT_NEEDED), looked up as the dynamic loader
// does (DT_RPATH of the object and of what loaded it, unless it has
// DT_RUNPATH, then DT_RUNPATH), and theirs in turn. plugins are libraries
// the program opens itself at run time (a ggml backend), which no
// DT_NEEDED names; their own libraries are walked the same way.
//
// What it guarantees: every file granted is beneath buildRoot both by the
// path the loader opens and by its real path (a symlink in the build that
// points out is refused), is named by an ELF's own DT_NEEDED or is a
// plugin, and has an ET_DYN header. It never grants a directory. A library
// found outside the build is the system's, which the policy's system paths
// already hold, and is not walked. It fails only when the program is not
// an ELF file it can read.
func ownLibraries(program string, plugins []string) (ownLibs, error) {
	real, err := filepath.EvalSymlinks(program)
	if err != nil {
		return ownLibs{}, err
	}
	root, home := buildRoot(real), filepath.Dir(real)
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
	for _, d := range append(rpathOf(exe, home), expandDirs(exe.runpath, home)...) {
		if d.origin && (d.path == root || within(root, d.path)) {
			out.origin = append(out.origin, d.path)
		}
	}
	granted, found := map[string]bool{}, map[string]bool{}
	grant := func(p string) {
		if !granted[p] {
			granted[p] = true
			out.files = append(out.files, p)
		}
	}
	// inBuild is the containment rule, for libraries and plugins alike:
	// the path the loader opens and the file behind it are both beneath
	// the build.
	inBuild := func(p string) (string, bool) {
		r, err := filepath.EvalSymlinks(p)
		return r, err == nil && within(root, p) && within(root, r)
	}
	exeRpath := rpathOf(exe, home)
	queue := []item{{path: real, o: exe}}
	for _, p := range plugins {
		r, ok := inBuild(p)
		if !ok {
			out.outside = append(out.outside, p)
			continue
		}
		o, err := readLib(r)
		if err != nil {
			out.skipped = append(out.skipped, p)
			continue
		}
		grant(p)
		grant(r)
		queue = append(queue, item{path: p, o: o, inherited: exeRpath})
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
		for _, needed := range it.o.needed {
			if found[needed] {
				continue // the loader loads a name once
			}
			// A name with a slash is a path: an absolute one is looked
			// for there alone; a relative one would be the working
			// directory's, not the build's, and is not looked for.
			name, cands := needed, dirs
			if strings.Contains(needed, "/") {
				cands = nil
				if filepath.IsAbs(needed) {
					cands = []searchDir{{path: filepath.Dir(needed)}}
					name = filepath.Base(needed)
				}
			}
			for _, d := range cands {
				cand := filepath.Join(d.path, name)
				if fi, err := os.Stat(cand); err != nil || fi.IsDir() {
					continue
				}
				found[needed] = true
				r, ok := inBuild(cand)
				if !ok {
					if d.origin && !systemPath(r) {
						out.outside = append(out.outside, cand)
					}
					break
				}
				o, err := readLib(r)
				if err != nil {
					out.skipped = append(out.skipped, cand)
					break
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

// ownLibrariesSafe is ownLibraries with a panic in the ELF parser (crafted
// input; debug/elf has had such bugs) turned into an error: Brainiac reads
// these files unconfined, and a file is then simply one it cannot read.
func ownLibrariesSafe(program string, plugins []string) (libs ownLibs, err error) {
	defer func() {
		if r := recover(); r != nil {
			libs, err = ownLibs{}, fmt.Errorf("reading its ELF failed: %v", r)
		}
	}()
	return ownLibraries(program, plugins)
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
