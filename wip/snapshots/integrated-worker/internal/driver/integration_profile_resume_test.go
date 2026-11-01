package driver

import (
	"encoding/json"
	"nomad-driver-lima/internal/lima"
	"nomad-driver-lima/internal/repositories"
	guestassets "nomad-driver-lima/plugin/guest"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestIntegrationProfileSchemaResumeProjectionAndTamper(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			root := t.TempDir()
			if err := os.Chmod(root, 0700); err != nil {
				t.Fatal(err)
			}
			store, err := openStore(root)
			if err != nil {
				t.Fatal(err)
			}
			source, next := resumeProjectionStates(store.Owner)
			if version == 2 {
				p := repositories.Profile{SchemaVersion: 1, ID: "cache", Version: "v1", NPM: &repositories.NPM{Registry: "https://cache.example.invalid/npm/"}}
				raw, err := repositories.Canonical(p)
				if err != nil {
					t.Fatal(err)
				}
				selected := &repositories.Selection{Profile: p, SHA256: repositories.Hash(raw), EnvironmentSHA256: strings.Repeat("e", 64), ExecutorSHA256: strings.Repeat("f", 64)}
				source.Version = 2
				next.Version = 2
				source.RepositoryProfile = selected
				copy := *selected
				next.RepositoryProfile = &copy
			}
			if err := validResumeIntent(source, next); err != nil {
				t.Fatal(err)
			}
			source.ResumeNext = next
			if err := store.write(source); err != nil {
				t.Fatal(err)
			}
			if err := store.reconcileResumeIntents(); err != nil {
				t.Fatal(err)
			}
			actual, err := store.read(next.Key)
			if err != nil || actual.Version != version || !reflect.DeepEqual(actual.RepositoryProfile, next.RepositoryProfile) {
				t.Fatal("schema/profile lost", err)
			}
			if version == 2 {
				changed := *next
				copy := *next.RepositoryProfile
				changed.RepositoryProfile = &copy
				copy.SHA256 = strings.Repeat("0", 64)
				if validResumeIntent(source, &changed) == nil {
					t.Fatal("changed profile accepted")
				}
				changed = *next
				changed.Version = 1
				if validResumeIntent(source, &changed) == nil {
					t.Fatal("schema downgrade accepted")
				}
			}
			raw, _ := json.Marshal(actual)
			if version == 1 && strings.Contains(string(raw), "RepositoryProfile") {
				t.Fatal("legacy disabled profile hash changed")
			}
		})
	}
}

func TestIntegrationProfileResumeReappliesPinnedProfileNotNewDefault(t *testing.T) {
	d, f, _, next, source, _ := stoppedResumeFixture(t)
	configureRepositoryFixture(t, d)
	selected, err := d.selectRepositories(TaskConfig{})
	if err != nil {
		t.Fatal(err)
	}
	selected.EnvironmentSHA256 = strings.Repeat("e", 64)
	selected.ExecutorSHA256 = lima.Hash([]byte(guestassets.RepositoryProfile))
	source.RepositoryProfile = selected
	source.Version = 2
	if err = d.store.write(source); err != nil {
		t.Fatal(err)
	}
	// A disabled/changed installation default cannot reselect this resumed disk.
	d.config.RepositoryProfilesFile = ""
	d.config.RepositoryProfilesSHA256 = ""
	r := &repositoryCLI{fakeCLI: f}
	d.client.Runner = r
	if _, _, err = d.StartTask(next); err != nil {
		t.Fatal(err)
	}
	actual, err := d.store.read(lima.Hash([]byte(next.ID)))
	if err != nil || actual.Version != 2 || actual.RepositoryProfile.SHA256 != selected.SHA256 || r.applied != 1 {
		t.Fatal("resume dropped pinned profile", err)
	}
	if _, err = d.ledger(); err != nil {
		t.Fatal("resume successor binding changed", err)
	}
}
