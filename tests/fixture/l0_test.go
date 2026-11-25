//go:build l0

package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/colony-2/c2j/pkg/recipe"
	"github.com/colony-2/c2j/pkg/workflowctl"
	"github.com/colony-2/jobdb/pkg/jobdb"
)

func TestL0GuestEvidence(t *testing.T) {
	name, vm := "l0-test", "001122aabbcc"
	plain := "PASS ordinary worker launch=" + name + " identity=" + vm + " capabilities=['docker']"
	disk := "L1 launch=" + name + " identity=" + vm + " boot=boot-id uid=1000 KVM API=12 VM created\nPASS L2 virtio disk boot 1\nPASS L2 virtio disk boot 2\nPASS L2 virtio disk boot 3\nPASS assigned worker KVM and three L2 disk boots"
	net := "\nPASS user-service peer, dynamic TAPs, peer and worker HTTP, DNS and outbound HTTP"
	dockerKVM := "\nPASS Docker with KVM in user service"
	for _, tc := range []struct {
		kvm, docker bool
		output      string
		pass        bool
	}{
		{false, false, plain, true}, {false, true, plain, false}, {false, true, plain + "\nPASS Docker in ordinary worker", true},
		{true, false, disk, false}, {true, false, disk + net, true}, {true, true, disk + net, false}, {true, true, disk + net + dockerKVM, true},
		{true, true, strings.Replace(disk+net+dockerKVM, "PASS L2 virtio disk boot 2", "", 1), false},
		{false, false, strings.Replace(plain, name, "foreign-launch", 1), false},
		{false, false, plain + "\n" + plain, false}, {true, false, plain, false},
	} {
		got, err := checkObservation(l0Case{kvm: tc.kvm, docker: tc.docker}, name, tc.output)
		if (err == nil) != tc.pass || tc.pass && got != vm {
			t.Fatalf("kvm=%t docker=%t evidence %q: %q, %v", tc.kvm, tc.docker, tc.output, got, err)
		}
	}
	for _, tc := range []struct {
		kvm, docker bool
		want        []string
	}{
		{true, true, []string{"ordinary", "docker", "kvm", "docker-kvm"}}, {false, true, []string{"ordinary", "docker"}}, {true, false, []string{"ordinary", "kvm"}}, {false, false, []string{"ordinary"}},
	} {
		var got []string
		for _, c := range l0Cases(tc.kvm, tc.docker) {
			got = append(got, c.name)
		}
		if !slices.Equal(got, tc.want) {
			t.Fatalf("matrix %v, expected %v", got, tc.want)
		}
	}
}

func TestL0RecipeAssignment(t *testing.T) {
	// Exercise the same recipe, artifact transfer and result reader using a
	// small kit and a local c2j, independent of hardware availability.
	f, ctx := setup(t)
	f.leaseDuration = time.Minute
	raw, err := os.ReadFile("../../scripts/kvm/worker-job.yaml")
	if err != nil {
		t.Fatal(err)
	}
	rec, err := recipe.LoadRecipeFromString(raw)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err = os.Mkdir(filepath.Join(dir, "kit"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "kit/worker.sh"), []byte("printf 'qualification artifact reached worker\\n'"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = command(ctx, dir, "tar", "czf", "worker.tar.gz", "kit"); err != nil {
		t.Fatal(err)
	}
	art, err := jobdb.NewArtifactFromFileNoCleanup("worker.tar.gz", filepath.Join(dir, "worker.tar.gz"))
	if err != nil {
		t.Fatal(err)
	}
	a, err := f.launchRecipe(ctx, "l0-test", "valid", "kvm", workload{}, *rec, workflowctl.StartJob{Inputs: map[string]any{"prompt": "l0-test"}, Artifacts: []jobdb.Artifact{art}})
	if err != nil {
		t.Fatal(err)
	}
	if err = runC2J(t, ctx, a); err != nil {
		t.Fatalf("execute private recipe: %v", err)
	}
	obs, err := f.l0Observation(ctx, "l0-test")
	if err != nil || strings.TrimSpace(obs) != "qualification artifact reached worker" {
		t.Fatalf("private recipe outcome %q, %v", obs, err)
	}
}
