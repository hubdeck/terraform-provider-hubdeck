package provider

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/hashicorp/terraform-plugin-framework/providerserver"
	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-testing/echoprovider"
	"github.com/hashicorp/terraform-plugin-testing/helper/resource"
	"github.com/hashicorp/terraform-plugin-testing/knownvalue"
	"github.com/hashicorp/terraform-plugin-testing/statecheck"
	"github.com/hashicorp/terraform-plugin-testing/tfjsonpath"
)

// stubKVault is a minimal in-memory simulation of the three KVault
// endpoints this provider talks to, enough to drive real plan/apply
// cycles through OpenTofu/Terraform without a live KVault instance.
// Mirrors the server-side behavior documented in KVault's own CLAUDE.md
// ("Fleet rebuilds (rekey)" section) closely enough for these tests:
// 0 existing machines by name -> create mode; 1 -> rekey (same keyId);
// an explicit keyId is honored as-is.
type stubKVault struct {
	mu       sync.Mutex
	machines map[string]string // name -> keyId
	revoked  map[string]bool   // keyId -> true
	secrets  map[string]string
}

func newStubKVault() *stubKVault {
	return &stubKVault{
		machines: map[string]string{},
		revoked:  map[string]bool{},
		secrets:  map[string]string{"demo-secret": "the-secret-value"},
	}
}

func (s *stubKVault) server(t *testing.T) *httptest.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/machine-keys/enroll-code", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Name  string `json:"name"`
			KeyID string `json:"keyId"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decoding enroll-code body: %v", err)
		}
		s.mu.Lock()
		defer s.mu.Unlock()

		keyID := body.KeyID
		if keyID == "" {
			keyID = s.machines[body.Name]
		}

		// Minting a code does NOT register the machine — same as the real
		// server (handleEnrollCode): a MachineKey is only ever created by
		// actual REDEMPTION (POST /enroll), which happens later,
		// out-of-band, on the host. Terraform may call Open() more than
		// once for the same config (e.g. once during plan, once during
		// apply) — minting twice must stay harmless and idempotent from
		// this stub's perspective, exactly as it is for the real server.
		mode := "create"
		if keyID != "" {
			mode = "rekey"
		} else {
			keyID = fmt.Sprintf("key-%s", body.Name)
		}

		w.WriteHeader(http.StatusCreated)
		resp := map[string]any{"code": "code-" + body.Name, "expiresInSeconds": 3600, "mode": mode}
		if mode == "rekey" {
			resp["keyId"] = keyID
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/api/machine-keys/resolve", func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		name := r.URL.Query().Get("name")
		keyID, ok := s.machines[name]
		if !ok || s.revoked[keyID] {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"keyId": keyID})
	})

	mux.HandleFunc("/api/machine-keys/", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		keyID := r.URL.Path[len("/api/machine-keys/"):]
		s.mu.Lock()
		s.revoked[keyID] = true
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"revoked": keyID})
	})

	mux.HandleFunc("/api/secret/", func(w http.ResponseWriter, r *http.Request) {
		name := r.URL.Path[len("/api/secret/"):]
		s.mu.Lock()
		value, ok := s.secrets[name]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "not found"})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"value": value})
	})

	return httptest.NewServer(mux)
}

func (s *stubKVault) isRevoked(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	keyID, ok := s.machines[name]
	return ok && s.revoked[keyID]
}

func protoV6ProviderFactories() map[string]func() (tfprotov6.ProviderServer, error) {
	return map[string]func() (tfprotov6.ProviderServer, error){
		"kvault": providerserver.NewProtocol6WithError(New("test")()),
		"echo":   echoprovider.NewProviderServer(),
	}
}

// TestMachineEnrollmentResource_Lifecycle drives a real create -> refresh
// (key_id resolves once the stub "host" redeems its code) -> destroy
// (revokes the key_id) cycle through kvault_machine_enrollment. This is
// the acceptance-level verification the code review asked for after the
// write-only-attribute redesign: confirms key_id genuinely resolves via
// GET /api/machine-keys/resolve and genuinely gets revoked via DELETE
// /api/machine-keys/:id, end to end through real Terraform/OpenTofu
// plan/apply/destroy cycles, not just unit-level Go calls.
func TestMachineEnrollmentResource_Lifecycle(t *testing.T) {
	stub := newStubKVault()
	srv := stub.server(t)
	defer srv.Close()

	config := fmt.Sprintf(`
provider "kvault" {
  endpoint         = %q
  ci_client_id     = "test-ci-id"
  ci_client_secret = "test-ci-secret"
}

resource "kvault_machine_enrollment" "host" {
  name = "host-1"
}
`, srv.URL)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		// No CheckDestroy here — the assertion that destroy actually
		// revokes is made below, via stub.isRevoked, after this whole
		// TestCase (including its automatic final destroy) has run.
		Steps: []resource.TestStep{
			{
				// Immediately after Create, no host has redeemed a code
				// yet — key_id must be empty, not an error.
				Config: config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kvault_machine_enrollment.host", "name", "host-1"),
					resource.TestCheckResourceAttr("kvault_machine_enrollment.host", "key_id", ""),
				),
			},
			{
				// Same config (no diff) — but PreConfig simulates the host
				// enrolling out-of-band (what a real redemption via
				// `kvault-cli enroll` would do server-side) just before
				// this step's plan, which always refreshes current state
				// first. Confirms Read actually calls the resolve
				// endpoint and picks up a key_id that appeared since the
				// last apply, not just at Create time.
				PreConfig: func() { stub.machines["host-1"] = "key-host-1" },
				Config:    config,
				Check: resource.ComposeAggregateTestCheckFunc(
					resource.TestCheckResourceAttr("kvault_machine_enrollment.host", "key_id", "key-host-1"),
				),
			},
		},
	})

	// The TestCase above auto-destroys everything it created on
	// completion — confirm that Delete actually revoked the key_id Read
	// resolved in the second step, not just removed it from state.
	if !stub.isRevoked("host-1") {
		t.Error("expected host-1's key to be revoked after destroy, but it wasn't")
	}
}

// TestMachineEnrollmentCodeEphemeralResource_NeverPersisted confirms the
// actual fix for the review's "Must fix" finding: the minted code reaches
// a consumer (here, the echo provider's own state attribute, the
// standard way to assert on an ephemeral value in these tests) and is
// never written to kvault_machine_enrollment's own state at all — there
// is no "code" attribute on that resource anymore for it to leak into.
func TestMachineEnrollmentCodeEphemeralResource_NeverPersisted(t *testing.T) {
	stub := newStubKVault()
	srv := stub.server(t)
	defer srv.Close()

	config := fmt.Sprintf(`
provider "kvault" {
  endpoint         = %q
  ci_client_id     = "test-ci-id"
  ci_client_secret = "test-ci-secret"
}

ephemeral "kvault_machine_enrollment_code" "host" {
  name = "host-2"
}

provider "echo" {
  data = ephemeral.kvault_machine_enrollment_code.host
}

resource "echo" "test" {}
`, srv.URL)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("echo.test", tfjsonpath.New("data").AtMapKey("code"), knownvalue.StringExact("code-host-2")),
					statecheck.ExpectKnownValue("echo.test", tfjsonpath.New("data").AtMapKey("mode"), knownvalue.StringExact("create")),
				},
			},
		},
	})
}

// TestSecretEphemeralResource_NeverPersisted is the same verification
// for the pre-existing kvault_secret ephemeral resource — not new from
// the review, but was never actually exercised through a real
// plan/apply cycle before (only the Go-level smoke test done during
// initial implementation), so added here for the same reason.
func TestSecretEphemeralResource_NeverPersisted(t *testing.T) {
	stub := newStubKVault()
	srv := stub.server(t)
	defer srv.Close()

	config := fmt.Sprintf(`
provider "kvault" {
  endpoint         = %q
  ci_client_id     = "test-ci-id"
  ci_client_secret = "test-ci-secret"
}

ephemeral "kvault_secret" "demo" {
  name = "demo-secret"
}

provider "echo" {
  data = ephemeral.kvault_secret.demo
}

resource "echo" "test" {}
`, srv.URL)

	resource.Test(t, resource.TestCase{
		IsUnitTest:               true,
		ProtoV6ProviderFactories: protoV6ProviderFactories(),
		Steps: []resource.TestStep{
			{
				Config: config,
				ConfigStateChecks: []statecheck.StateCheck{
					statecheck.ExpectKnownValue("echo.test", tfjsonpath.New("data").AtMapKey("value"), knownvalue.StringExact("the-secret-value")),
				},
			},
		},
	})
}
