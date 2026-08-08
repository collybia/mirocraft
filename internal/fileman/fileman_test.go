package fileman

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// newRoot builds a sandbox with a sibling directory outside it, which is what
// every escape attempt below tries to reach.
func newRoot(t *testing.T) (*Root, string) {
	t.Helper()

	base := t.TempDir()
	inside := filepath.Join(base, "server")
	outside := filepath.Join(base, "secrets")

	for _, dir := range []string{inside, outside} {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	if err := os.WriteFile(filepath.Join(outside, "passwords.txt"), []byte("do not read me"), 0o600); err != nil {
		t.Fatalf("writing the bait: %v", err)
	}

	root, err := NewRoot(inside)
	if err != nil {
		t.Fatalf("NewRoot: %v", err)
	}
	return root, outside
}

// --- traversal, the mandatory part ---

// Every one of these must be refused. They are the shapes a traversal
// actually takes in the wild, not a token sample.
func TestResolveRefusesTraversal(t *testing.T) {
	root, _ := newRoot(t)

	hostile := []string{
		"..",
		"../",
		"../secrets",
		"../secrets/passwords.txt",
		"../../etc/passwd",
		"world/../../secrets",
		"world/../../../etc/passwd",
		"./../secrets",
		"a/b/c/../../../../secrets",

		// Backslashes, because they are separators on Windows and a check
		// that only looks for "/" sails straight past them.
		"..\\secrets",
		"..\\..\\etc\\passwd",
		"world\\..\\..\\secrets",

		// Absolute paths in every spelling.
		"C:\\Windows\\System32",
		"C:/Windows/System32",
		"//server/share",
		"\\\\server\\share",

		// A null byte truncates the path in a C API underneath.
		"world\x00.txt",
		"\x00",
	}

	for _, p := range hostile {
		t.Run(p, func(t *testing.T) {
			got, err := root.Resolve(p)
			if err == nil {
				t.Fatalf("Resolve(%q) = %q, want it refused", p, got)
			}
		})
	}
}

// Ordinary paths must keep working, or the defence is useless in practice.
func TestResolveAllowsNormalPaths(t *testing.T) {
	root, _ := newRoot(t)

	fine := map[string]string{
		"":                         "",
		"/":                        "",
		".":                        "",
		"server.properties":        "server.properties",
		"/server.properties":       "server.properties",
		"world/level.dat":          filepath.Join("world", "level.dat"),
		"/plugins/EssentialsX.jar": filepath.Join("plugins", "EssentialsX.jar"),
		"./world/region":           filepath.Join("world", "region"),
		"world//region":            filepath.Join("world", "region"),
		"a/b/../c":                 filepath.Join("a", "c"),
		"файл.txt":                 "файл.txt",
		"with space.txt":           "with space.txt",
	}

	for input, want := range fine {
		t.Run(input, func(t *testing.T) {
			got, err := root.Resolve(input)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", input, err)
			}
			expected := filepath.Join(root.Dir(), want)
			if got != expected {
				t.Fatalf("Resolve(%q) = %q, want %q", input, got, expected)
			}
		})
	}
}

// The subtle one: a symlink inside the root pointing out of it. A string
// check passes this, which is why resolution has to touch the disk.
func TestResolveRefusesASymlinkOutOfTheRoot(t *testing.T) {
	root, outside := newRoot(t)

	link := filepath.Join(root.Dir(), "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	for _, p := range []string{"escape", "escape/passwords.txt", "/escape/passwords.txt"} {
		if got, err := root.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) = %q, want it refused — it leads outside the root", p, got)
		}
	}
}

// A symlink pointing at a file inside the root is fine and must keep working.
func TestResolveAllowsASymlinkInsideTheRoot(t *testing.T) {
	root, _ := newRoot(t)

	target := filepath.Join(root.Dir(), "real.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}
	link := filepath.Join(root.Dir(), "alias.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	if _, err := root.Resolve("alias.txt"); err != nil {
		t.Fatalf("Resolve of a link inside the root: %v", err)
	}
}

// Creating a file through a symlinked directory that leads out must fail too,
// even though the file itself does not exist yet.
func TestResolveRefusesCreatingThroughAnEscapingSymlink(t *testing.T) {
	root, outside := newRoot(t)

	link := filepath.Join(root.Dir(), "out")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	if got, err := root.Resolve("out/newfile.txt"); err == nil {
		t.Fatalf("Resolve = %q, want it refused — the parent leads outside", got)
	}
}

// Windows silently maps these onto devices and strips trailing dots and
// spaces, so a file created as one name would be reachable as another.
func TestResolveRefusesWindowsHazards(t *testing.T) {
	root, _ := newRoot(t)

	hazards := []string{
		"CON", "con", "NUL", "aux.txt", "COM1", "lpt9.log",
		"notes.txt:hidden",
		"trailing.",
		"trailing ",
		"dir/CON",
	}

	for _, p := range hazards {
		if got, err := root.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) = %q, want it refused", p, got)
		}
	}
}

func TestNewRootRejectsNonsense(t *testing.T) {
	if _, err := NewRoot(""); err == nil {
		t.Error("NewRoot accepted an empty path")
	}
	if _, err := NewRoot(filepath.Join(t.TempDir(), "does-not-exist")); err == nil {
		t.Error("NewRoot accepted a path that does not exist")
	}

	file := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if _, err := NewRoot(file); err == nil {
		t.Error("NewRoot accepted a file as a root")
	}
}

// A root reached through a symlink — a data directory on a mount, say — must
// still accept its own contents.
func TestNewRootResolvesASymlinkedRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.MkdirAll(real, 0o750); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}
	if err := os.WriteFile(filepath.Join(real, "server.properties"), []byte("x"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	root, err := NewRoot(link)
	if err != nil {
		t.Fatalf("NewRoot on a symlinked root: %v", err)
	}
	if _, err := root.ResolveExisting("server.properties"); err != nil {
		t.Fatalf("a file in a symlinked root is unreachable: %v", err)
	}
}

func TestRel(t *testing.T) {
	root, outside := newRoot(t)

	got, err := root.Rel(filepath.Join(root.Dir(), "world", "level.dat"))
	if err != nil {
		t.Fatalf("Rel: %v", err)
	}
	if got != "/world/level.dat" {
		t.Fatalf("Rel = %q, want /world/level.dat", got)
	}

	if got, err := root.Rel(root.Dir()); err != nil || got != "/" {
		t.Fatalf("Rel of the root = %q (%v), want /", got, err)
	}
	if _, err := root.Rel(outside); err == nil {
		t.Fatal("Rel accepted a path outside the root")
	}
}

// --- operations ---

func seed(t *testing.T, root *Root) {
	t.Helper()

	files := map[string]string{
		"server.properties":  "motd=hi\nmax-players=20\n",
		"eula.txt":           "eula=true\n",
		"world/level.dat":    "not really nbt",
		"plugins/readme.txt": "put jars here",
	}
	for rel, content := range files {
		abs := filepath.Join(root.Dir(), filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(abs), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o640); err != nil {
			t.Fatalf("writing %s: %v", rel, err)
		}
	}
}

func TestList(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	entries, err := root.List("/")
	if err != nil {
		t.Fatalf("List: %v", err)
	}

	// Directories first, then files, each alphabetically.
	var names []string
	for _, e := range entries {
		names = append(names, e.Name)
	}
	want := []string{"plugins", "world", "eula.txt", "server.properties"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("listing = %v, want %v", names, want)
	}

	for _, e := range entries {
		if e.Path == "" || !strings.HasPrefix(e.Path, "/") {
			t.Errorf("entry %q has path %q, want a rooted path", e.Name, e.Path)
		}
		if e.Name == "world" && e.Type != TypeDirectory {
			t.Errorf("world is typed %q", e.Type)
		}
		if e.Name == "eula.txt" && e.Type != TypeFile {
			t.Errorf("eula.txt is typed %q", e.Type)
		}
	}
}

func TestListRefusesAFileAndMissingPaths(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	if _, err := root.List("/eula.txt"); !errors.Is(err, ErrNotADirEntry) {
		t.Errorf("List of a file = %v, want ErrNotADirEntry", err)
	}
	if _, err := root.List("/nope"); !errors.Is(err, ErrNotFound) {
		t.Errorf("List of a missing path = %v, want ErrNotFound", err)
	}
	if _, err := root.List("../secrets"); !errors.Is(err, ErrEscapes) {
		t.Errorf("List outside the root = %v, want ErrEscapes", err)
	}
}

func TestReadAndWriteText(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	body, err := root.ReadText("/server.properties")
	if err != nil {
		t.Fatalf("ReadText: %v", err)
	}
	if !strings.Contains(body, "max-players=20") {
		t.Fatalf("content = %q", body)
	}

	if err := root.WriteText("/server.properties", "motd=changed\n"); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	again, err := root.ReadText("/server.properties")
	if err != nil {
		t.Fatalf("ReadText after write: %v", err)
	}
	if again != "motd=changed\n" {
		t.Fatalf("content after write = %q", again)
	}

	// Writing somewhere new must create the parents.
	if err := root.WriteText("/config/deep/new.yml", "a: b\n"); err != nil {
		t.Fatalf("WriteText into a new tree: %v", err)
	}
	if _, err := root.ReadText("/config/deep/new.yml"); err != nil {
		t.Fatalf("reading it back: %v", err)
	}
}

// Handing a jar to a text editor produces a screen of replacement characters,
// and saving it back would corrupt the file.
func TestReadTextRefusesBinary(t *testing.T) {
	root, _ := newRoot(t)

	jar := filepath.Join(root.Dir(), "plugin.jar")
	if err := os.WriteFile(jar, []byte{0x50, 0x4B, 0x03, 0x04, 0x00, 0x00, 0x01}, 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := root.ReadText("/plugin.jar"); !errors.Is(err, ErrBinary) {
		t.Fatalf("ReadText of a binary = %v, want ErrBinary", err)
	}
}

func TestReadTextRefusesOversized(t *testing.T) {
	root, _ := newRoot(t)

	big := filepath.Join(root.Dir(), "huge.log")
	if err := os.WriteFile(big, make([]byte, MaxTextBytes+1), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}

	if _, err := root.ReadText("/huge.log"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("ReadText of an oversized file = %v, want ErrTooLarge", err)
	}
	if err := root.WriteText("/huge.log", strings.Repeat("x", MaxTextBytes+1)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("WriteText of oversized content = %v, want ErrTooLarge", err)
	}
}

func TestUpload(t *testing.T) {
	root, _ := newRoot(t)

	n, err := root.Upload("/plugins/new.jar", strings.NewReader("jar bytes"))
	if err != nil {
		t.Fatalf("Upload: %v", err)
	}
	if n != int64(len("jar bytes")) {
		t.Fatalf("wrote %d bytes", n)
	}

	if _, err := root.Upload("../escape.jar", strings.NewReader("x")); !errors.Is(err, ErrEscapes) {
		t.Fatalf("Upload outside the root = %v, want ErrEscapes", err)
	}
}

func TestMkdirAndRemove(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	if err := root.Mkdir("/plugins/config"); err != nil {
		t.Fatalf("Mkdir: %v", err)
	}
	if err := root.Mkdir("/plugins/config"); !errors.Is(err, ErrExists) {
		t.Fatalf("Mkdir of an existing path = %v, want ErrExists", err)
	}

	if err := root.Remove("/plugins"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := root.Stat("/plugins"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("the directory survived removal: %v", err)
	}
}

// Deleting the root would leave a server with no directory at all.
func TestRemoveRefusesTheRootItself(t *testing.T) {
	root, _ := newRoot(t)

	for _, p := range []string{"/", "", "."} {
		if err := root.Remove(p); !errors.Is(err, ErrInvalidPath) {
			t.Errorf("Remove(%q) = %v, want it refused", p, err)
		}
	}
	if _, err := os.Stat(root.Dir()); err != nil {
		t.Fatalf("the root was deleted: %v", err)
	}
}

func TestMoveAndCopy(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	if err := root.Move("/eula.txt", "/backup/eula.txt"); err != nil {
		t.Fatalf("Move: %v", err)
	}
	if _, err := root.Stat("/eula.txt"); !errors.Is(err, ErrNotFound) {
		t.Error("the source survived the move")
	}
	if _, err := root.Stat("/backup/eula.txt"); err != nil {
		t.Fatalf("the destination is missing: %v", err)
	}

	if err := root.Copy("/world", "/world-copy"); err != nil {
		t.Fatalf("Copy: %v", err)
	}
	if _, err := root.Stat("/world-copy/level.dat"); err != nil {
		t.Fatalf("the copied tree is incomplete: %v", err)
	}
	if _, err := root.Stat("/world/level.dat"); err != nil {
		t.Fatalf("the source tree was damaged: %v", err)
	}
}

func TestMoveAndCopyRefuseEscapes(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	cases := []struct{ from, to string }{
		{"/eula.txt", "../escaped.txt"},
		{"../secrets/passwords.txt", "/stolen.txt"},
	}

	for _, tc := range cases {
		if err := root.Move(tc.from, tc.to); err == nil {
			t.Errorf("Move(%q, %q) succeeded, want it refused", tc.from, tc.to)
		}
		if err := root.Copy(tc.from, tc.to); err == nil {
			t.Errorf("Copy(%q, %q) succeeded, want it refused", tc.from, tc.to)
		}
	}
}

// Moving a directory into itself produces a subtree nothing can reach.
func TestMoveRefusesMovingIntoItself(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	if err := root.Move("/world", "/world/inner"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Move into itself = %v, want it refused", err)
	}
}

func TestMoveRefusesOverwriting(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	if err := root.Move("/eula.txt", "/server.properties"); !errors.Is(err, ErrExists) {
		t.Fatalf("Move onto an existing file = %v, want ErrExists", err)
	}
}

// --- archives ---

func TestArchiveAndUnarchive(t *testing.T) {
	root, _ := newRoot(t)
	seed(t, root)

	name, err := root.Archive([]string{"/world", "/server.properties"}, "/backup.zip")
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	if name != "/backup.zip" {
		t.Fatalf("archive path = %q", name)
	}

	if err := root.Unarchive("/backup.zip", "/restored"); err != nil {
		t.Fatalf("Unarchive: %v", err)
	}
	if _, err := root.Stat("/restored/world/level.dat"); err != nil {
		t.Fatalf("the restored tree is incomplete: %v", err)
	}
	if _, err := root.Stat("/restored/server.properties"); err != nil {
		t.Fatalf("the restored file is missing: %v", err)
	}
}

// Zip Slip: an uploaded archive whose members climb out of the destination.
// This is the reason unarchiving is the most dangerous operation here.
func TestUnarchiveRefusesTraversingMembers(t *testing.T) {
	root, _ := newRoot(t)

	archivePath := filepath.Join(root.Dir(), "evil.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	writer := zip.NewWriter(file)
	for _, name := range []string{"../../escaped.txt", "..\\..\\escaped2.txt"} {
		w, err := writer.Create(name)
		if err != nil {
			t.Fatalf("creating entry: %v", err)
		}
		if _, err := w.Write([]byte("owned")); err != nil {
			t.Fatalf("writing entry: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("closing: %v", err)
	}
	_ = file.Close()

	if err := root.Unarchive("/evil.zip", "/unpacked"); err == nil {
		t.Fatal("Unarchive accepted an archive that escapes the destination")
	}

	// And nothing must have been written outside.
	outsideBase := filepath.Dir(root.Dir())
	for _, name := range []string{"escaped.txt", "escaped2.txt"} {
		if _, err := os.Stat(filepath.Join(outsideBase, name)); !os.IsNotExist(err) {
			t.Errorf("%s was written outside the root", name)
		}
	}
}

func TestUnarchiveTarGzRefusesTraversingMembers(t *testing.T) {
	root, _ := newRoot(t)

	archivePath := filepath.Join(root.Dir(), "evil.tar.gz")
	file, err := os.Create(archivePath)
	if err != nil {
		t.Fatalf("creating: %v", err)
	}
	gz := gzip.NewWriter(file)
	tw := tar.NewWriter(gz)
	body := []byte("owned")
	if err := tw.WriteHeader(&tar.Header{
		Name: "../../escaped.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatalf("header: %v", err)
	}
	if _, err := tw.Write(body); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = tw.Close()
	_ = gz.Close()
	_ = file.Close()

	if err := root.Unarchive("/evil.tar.gz", "/unpacked"); err == nil {
		t.Fatal("Unarchive accepted a tar that escapes the destination")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(root.Dir()), "escaped.txt")); !os.IsNotExist(err) {
		t.Error("the traversing member was written outside the root")
	}
}

func TestUnarchiveRefusesUnknownFormats(t *testing.T) {
	root, _ := newRoot(t)

	if err := os.WriteFile(filepath.Join(root.Dir(), "thing.rar"), []byte("x"), 0o640); err != nil {
		t.Fatalf("writing: %v", err)
	}
	if err := root.Unarchive("/thing.rar", "/out"); !errors.Is(err, ErrInvalidPath) {
		t.Fatalf("Unarchive of a rar = %v, want it refused", err)
	}
}

// Following a symlink while archiving would pull a file from outside the root
// into an archive the client can then download.
func TestArchiveSkipsSymlinks(t *testing.T) {
	root, outside := newRoot(t)
	seed(t, root)

	link := filepath.Join(root.Dir(), "world", "leak")
	if err := os.Symlink(outside, link); err != nil {
		t.Skipf("symlinks are not available here: %v", err)
	}

	if _, err := root.Archive([]string{"/world"}, "/out.zip"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	reader, err := zip.OpenReader(filepath.Join(root.Dir(), "out.zip"))
	if err != nil {
		t.Fatalf("opening the archive: %v", err)
	}
	defer func() { _ = reader.Close() }()

	for _, entry := range reader.File {
		if strings.Contains(entry.Name, "passwords") {
			t.Fatalf("the archive contains a file from outside the root: %q", entry.Name)
		}
	}
}

func TestIsBinary(t *testing.T) {
	if isBinary([]byte("motd=hello\nmax-players=20\n")) {
		t.Error("a properties file was judged binary")
	}
	if !isBinary([]byte{0x50, 0x4B, 0x00, 0x01}) {
		t.Error("a file with a null byte was judged text")
	}
	if isBinary(nil) {
		t.Error("an empty file was judged binary")
	}
	if isBinary([]byte("кириллица и юникод — тоже текст")) {
		t.Error("UTF-8 text was judged binary")
	}
}

// A path that is only reachable because the check was skipped on Windows
// would still be reachable on Linux, so the rules do not vary by host.
func TestRulesDoNotDependOnTheHost(t *testing.T) {
	root, _ := newRoot(t)

	// These are refused everywhere, including on Linux where they would
	// otherwise be legal file names.
	for _, p := range []string{"CON", "a:b", "x."} {
		if _, err := root.Resolve(p); err == nil {
			t.Errorf("Resolve(%q) was allowed on %s; the rules must not vary by host",
				p, runtime.GOOS)
		}
	}
}
