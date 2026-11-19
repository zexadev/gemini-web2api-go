package app

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHydrogenRouting(t *testing.T) {
	withPoolCookie(t)
	for _, tc := range []struct {
		name       string
		think      int
		wantHeader string
	}{
		{"gemini-4-hydrogen", 1, "CAEqEDU2ZmRkMTk5MzEyODE1ZTJ4AYABAQ=="},
		{"gemini-4-hydrogen-thinking", 2, "CAEqEDU2ZmRkMTk5MzEyODE1ZTJ4AYABAg=="},
	} {
		t.Run(tc.name, func(t *testing.T) {
			name, mc, err := resolveModel(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			// Paid-account Hydrogen keeps the 3.8 Flash route (upstream #74).
			if name != tc.name || mc.HexID != "56fdd199312815e2" || mc.Mode != 1 ||
				mc.thinkWire() != tc.think || mc.Upstream != "4 Hydrogen" {
				t.Fatalf("unexpected Hydrogen route: name=%s config=%+v", name, mc)
			}
			if !modelNeedsLogin(mc) {
				t.Fatal("Hydrogen must use a signed-in account")
			}
			var header []interface{}
			if err := json.Unmarshal([]byte(buildModelHeader(mc.HexID, mc.Mode, mc.thinkWire(), "test-uuid")), &header); err != nil {
				t.Fatal(err)
			}
			if len(header) != 17 || header[4] != "56fdd199312815e2" ||
				header[14] != float64(1) || header[15] != float64(tc.think) {
				t.Fatalf("incorrect StreamGenerate model header: %v", header)
			}
			if got := wcModelHeaderBin(mc.HexID, mc.Mode, mc.thinkWire()); got != tc.wantHeader {
				t.Errorf("WebChannel model header = %s, want %s", got, tc.wantHeader)
			}
		})
	}
}

func TestHydrogenCookieGateAndModelLists(t *testing.T) {
	for _, signedIn := range []bool{false, true} {
		label := "anonymous"
		if signedIn {
			label = "signed-in"
		}
		t.Run(label, func(t *testing.T) {
			if signedIn {
				withPoolCookie(t)
			}
			for _, name := range []string{"gemini-4-hydrogen", "gemini-4-hydrogen-thinking"} {
				_, _, err := resolveModel(name)
				if signedIn && err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !signedIn && (err == nil || !strings.Contains(err.Error(), "cookie")) {
					t.Fatalf("%s: anonymous selection must explain cookie requirement, got %v", name, err)
				}
				for _, endpoint := range []struct {
					path   string
					handle http.HandlerFunc
					entry  string
				}{
					{"/v1/models", handleModels, `"id":"` + name + `"`},
					{"/v1beta/models", handleGeminiModels, `"name":"models/` + name + `"`},
				} {
					w := httptest.NewRecorder()
					endpoint.handle(w, httptest.NewRequest(http.MethodGet, endpoint.path, nil))
					if w.Code != http.StatusOK {
						t.Fatalf("%s: status %d", endpoint.path, w.Code)
					}
					if listed := strings.Contains(w.Body.String(), endpoint.entry); listed != signedIn {
						t.Errorf("%s lists %s = %v, want %v", endpoint.path, name, listed, signedIn)
					}
				}
			}
		})
	}
}
