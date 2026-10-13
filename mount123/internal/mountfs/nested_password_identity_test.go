package mountfs

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestEncryptedOuter7zPreservesNestedPasswordIdentity(t *testing.T) {
	seven, err := exec.LookPath("7z")
	if err != nil {
		t.Skip("7z fixture generator unavailable")
	}
	dir := t.TempDir()
	inner := multilevelFixture(t, "zip", map[string][]byte{"payload.txt": []byte("synthetic payload")})
	if err := os.WriteFile(filepath.Join(dir, "inner.zip"), inner, 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(t.TempDir(), "outer.7z")
	cmd := exec.Command(seven, "a", "-t7z", "-mhe=on", "-psecret", output, "inner.zip")
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("synthetic fixture generation: %v %s", err, out)
	}
	data, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	root, base := otherArchiveFixture(t, "outer.7z", data, []byte("secret"), false)
	root.tree.api = &nestedPasswordAPI{archiveAPI: base}
	service := &Service{tree: root.tree}
	outer := lookup(t, root, "outer.7z")
	idx, source, a, err := outer.archiveDirectoryIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The nil-index path is the previous FUSE behavior, which rediscovered the
	// outer header password before constructing its root view identity.
	legacy, err := root.tree.buildNestedArchive(context.Background(), source, a, nil, "inner.zip", idx.members["inner.zip"])
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	current := lookup(t, outer, "inner.zip").item.nested
	// The public Service path seeds its root identity with the same header
	// password. Both frontends must continue to share the legacy cache chain.
	serviceIndex, serviceArchive, serviceSource, password, err := service.outerArchiveIndex(context.Background(), 77)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(password)
	serviceParent, err := root.tree.rootArchiveView(context.Background(), serviceSource, serviceArchive, serviceIndex, password)
	if err != nil {
		t.Fatal(err)
	}
	defer serviceParent.Close()
	serviceChild, err := root.tree.enterNestedArchive(context.Background(), serviceParent, "inner.zip", serviceIndex.members["inner.zip"])
	if err != nil {
		t.Fatal(err)
	}
	defer serviceChild.Close()
	multilevelNames(t, service, "inner.zip", "payload.txt")
	multilevelRead(t, service, "inner.zip/payload.txt", []byte("synthetic payload"))
	if current.parent.identity != legacy.parent.identity || current.cachePrefix != legacy.cachePrefix || serviceChild.cachePrefix != legacy.cachePrefix {
		t.Fatal("pinned encrypted outer index changed existing nested cache identities")
	}
}
