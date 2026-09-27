package rules

import (
	"reflect"
	"testing"

	"github.com/alam0rt/mergegate"
)

func patch(p string) mergegate.PullRequest {
	return mergegate.PullRequest{Files: []mergegate.File{{Path: "f", Status: "modified", Patch: p}}}
}

func TestVersionChanges(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  []string // "old->new:kind"
	}{
		{"flux tag patch", "-    newTag: v1.12.27 # {\"$imagepolicy\": \"x\"}\n+    newTag: v1.12.29 # {\"$imagepolicy\": \"x\"}", []string{"v1.12.27->v1.12.29:patch"}},
		{"image minor", "-      image: docker.io/velero/velero:v1.14.2\n+      image: docker.io/velero/velero:v1.15.0", []string{"v1.14.2->v1.15.0:minor"}},
		{"major", "-    newTag: 1.9.3\n+    newTag: 2.0.0", []string{"1.9.3->2.0.0:major"}},
		{"zero minor is major", "-    newTag: v0.28.0\n+    newTag: v0.29.1", []string{"v0.28.0->v0.29.1:major"}},
		{"zero patch is patch", "-    newTag: v0.29.3\n+    newTag: v0.29.4", []string{"v0.29.3->v0.29.4:patch"}},
		{"downgrade", "-    newTag: v1.12.29\n+    newTag: v1.12.27", []string{"v1.12.29->v1.12.27:downgrade"}},
		{"go.sum pairs despite hashes",
			"-golang.org/x/text v0.41.0 h1:AAA=\n-golang.org/x/text v0.41.0/go.mod h1:BBB=\n+golang.org/x/text v0.42.0 h1:CCC=\n+golang.org/x/text v0.42.0/go.mod h1:DDD=",
			[]string{"v0.41.0->v0.42.0:major", "v0.41.0->v0.42.0:major"}},
		{"different names do not pair", "-    image: a:1.0.0\n+    image: b:3.0.0", nil},
		{"non-semver tags ignored", "-    newTag: 1920-fe70e951a3c5\n+    newTag: 1961-96d39adbc812", nil},
		{"ip addresses ignored", "-  host: 10.0.0.1\n+  host: 10.0.0.2", nil},
		{"unchanged version ignored", "-  image: a:1.0.0 # old\n+  image: a:1.0.0 # new", nil},
		{"context lines ignored", " image: a:1.0.0\n-x: 1\n+x: 2", nil},
		{"blocks pair separately",
			"-  a: v1.0.0\n+  a: v1.0.1\n unchanged\n-  b: v2.0.0\n+  b: v3.0.0",
			[]string{"v1.0.0->v1.0.1:patch", "v2.0.0->v3.0.0:major"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, ch := range VersionChanges(patch(c.patch), true) {
				got = append(got, ch.Old+"->"+ch.New+":"+ch.Kind)
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestCheckFlagsMajorAndDowngrade(t *testing.T) {
	gosum := patch("-a v0.1.0 h1:A=\n-a v0.1.0/go.mod h1:B=\n+a v0.2.0 h1:C=\n+a v0.2.0/go.mod h1:D=")
	if got := Check(Default(), gosum).RiskyVersions; len(got) != 1 {
		t.Errorf("go.sum lists each module twice; want one reason, got %q", got)
	}
	f := Check(Default(), patch("-  newTag: v0.28.0\n+  newTag: v0.29.0\n unchanged\n-  x: 1.2.0\n+  x: 1.1.0"))
	if len(f.RiskyVersions) != 2 {
		t.Errorf("RiskyVersions = %q", f.RiskyVersions)
	}
	cfg := Default()
	cfg.Versions.ZeroMinorIsMajor = false
	if f := Check(cfg, patch("-  newTag: v0.28.0\n+  newTag: v0.29.0")); len(f.RiskyVersions) != 0 {
		t.Errorf("with zero_minor_is_major off, 0.28->0.29 is a minor: %q", f.RiskyVersions)
	}
}
