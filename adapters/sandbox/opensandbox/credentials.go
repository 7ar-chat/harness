package opensandbox

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/GhaziBenDahmane/harness/core/sandbox"

	sdk "github.com/alibaba/OpenSandbox/sdks/sandbox/go"
)

// brokerInstance is an osbInstance created with the Credential Vault enabled. Only these expose the broker, so a
// sandbox without the credential proxy is never mistaken for one that can keep a secret out of the workload.
type brokerInstance struct{ *osbInstance }

var _ sandbox.CredentialBroker = brokerInstance{}

// SetCredentials makes the egress sidecar's Credential Vault equal to set. The first call creates the vault; later
// calls replace it in one atomic patch, so a renewed token never leaves a window in which requests go out
// unauthenticated. Concurrent callers (two runs of one thread) can collide on the revision; the loser reads the new
// state and tries again.
func (s brokerInstance) SetCredentials(ctx context.Context, set sandbox.CredentialSet) error {
	want := sdkVault(set)
	var err error
	for range 3 {
		if err = s.applyVault(ctx, want); err == nil || !vaultConflict(err) {
			break
		}
	}
	if err != nil {
		return fmt.Errorf("credential vault: %w", err)
	}
	return nil
}

func (s *osbInstance) applyVault(ctx context.Context, want sdk.CredentialVaultCreateRequest) error {
	state, err := s.sb.GetCredentialVault(ctx)
	if err != nil {
		if !vaultMissing(err) {
			return err
		}
		_, err := s.sb.CreateCredentialVault(ctx, want)
		return err
	}
	// Existing names are replaced, new ones added, those no longer wanted deleted (an add of an existing name is
	// refused). The whole change is one atomic patch guarded by the revision just read.
	haveCreds, haveBinds := map[string]bool{}, map[string]bool{}
	for _, c := range state.Credentials {
		haveCreds[c.Name] = true
	}
	for _, b := range state.Bindings {
		haveBinds[b.Name] = true
	}
	var cm sdk.CredentialMutationSet
	var bm sdk.CredentialBindingMutationSet
	wantCreds, wantBinds := map[string]bool{}, map[string]bool{}
	for _, c := range want.Credentials {
		wantCreds[c.Name] = true
		if haveCreds[c.Name] {
			cm.Replace = append(cm.Replace, c)
		} else {
			cm.Add = append(cm.Add, c)
		}
	}
	for _, b := range want.Bindings {
		wantBinds[b.Name] = true
		if haveBinds[b.Name] {
			bm.Replace = append(bm.Replace, b)
		} else {
			bm.Add = append(bm.Add, b)
		}
	}
	for n := range haveCreds {
		if !wantCreds[n] {
			cm.Delete = append(cm.Delete, n)
		}
	}
	for n := range haveBinds {
		if !wantBinds[n] {
			bm.Delete = append(bm.Delete, n)
		}
	}
	rev := state.Revision
	_, err = s.sb.PatchCredentialVault(ctx, sdk.CredentialVaultPatchRequest{ExpectedRevision: &rev, Credentials: &cm, Bindings: &bm})
	return err
}

// vaultMissing reports "this sandbox has no vault yet" (404 from the sidecar), as opposed to a gone sandbox or a
// transport failure.
func vaultMissing(err error) bool {
	var api *sdk.APIError
	return errors.As(err, &api) && api.StatusCode == http.StatusNotFound
}

// vaultConflict reports a lost race: the vault appeared between the read and the create, or its revision moved
// between the read and the patch.
func vaultConflict(err error) bool {
	var api *sdk.APIError
	return errors.As(err, &api) && api.StatusCode == http.StatusConflict
}

func sdkVault(set sandbox.CredentialSet) sdk.CredentialVaultCreateRequest {
	var out sdk.CredentialVaultCreateRequest
	for _, c := range set.Credentials {
		out.Credentials = append(out.Credentials, sdk.Credential{Name: c.Name, Source: sdk.InlineCredentialSource{Value: c.Value}})
	}
	for _, b := range set.Bindings {
		auth := sdk.CredentialAuth{Type: sdk.CredentialAuthType(b.Auth), Credential: b.Credential}
		out.Bindings = append(out.Bindings, sdk.CredentialBinding{
			Name:  b.Name,
			Match: sdk.CredentialMatch{Schemes: []sdk.CredentialScheme{sdk.CredentialSchemeHTTPS}, Hosts: []string{b.Host}, Methods: b.Methods, Paths: b.Paths},
			Auth:  auth,
		})
	}
	return out
}
