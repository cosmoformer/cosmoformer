package coordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"github.com/cosmoformer/cosmoformer/internal/runs"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	"sort"
	"strings"
)

// Lock bytes remain custody artifacts; parsing only checks that reported
// external packages are exactly the native lock selections, never reserializes.
func verifyProviderLock(m *runs.Manifest, b []byte) error {
	if !m.Lock.Present {
		if b != nil {
			return runs.Fail("PROVIDER_LOCK_INVALID")
		}
		return nil
	}
	h := sha256.Sum256(b)
	if m.Lock.Hash != hex.EncodeToString(h[:]) || m.Lock.Size != int64(len(b)) {
		return runs.Fail("PROVIDER_LOCK_INVALID")
	}
	file, diags := hclsyntax.ParseConfig(b, "provider-lock", hcl.InitialPos)
	if diags.HasErrors() {
		return runs.Fail("PROVIDER_LOCK_INVALID")
	}
	body := file.Body.(*hclsyntax.Body)
	if len(body.Attributes) > 0 {
		return runs.Fail("PROVIDER_LOCK_INVALID")
	}
	providers := []runs.Provider{}
	seen := map[string]bool{}
	for _, block := range body.Blocks {
		if block.Type != "provider" || len(block.Labels) != 1 || seen[block.Labels[0]] || len(block.Body.Blocks) > 0 {
			return runs.Fail("PROVIDER_LOCK_INVALID")
		}
		seen[block.Labels[0]] = true
		p := runs.Provider{Address: block.Labels[0], Platform: "linux_amd64", Hashes: []string{}}
		for name, a := range block.Body.Attributes {
			v, d := a.Expr.Value(nil)
			if d.HasErrors() || !v.IsKnown() || v.IsNull() {
				return runs.Fail("PROVIDER_LOCK_INVALID")
			}
			switch name {
			case "version":
				if v.Type() != cty.String {
					return runs.Fail("PROVIDER_LOCK_INVALID")
				}
				p.Version = v.AsString()
			case "constraints":
				if v.Type() != cty.String {
					return runs.Fail("PROVIDER_LOCK_INVALID")
				}
			case "hashes":
				if !v.CanIterateElements() {
					return runs.Fail("PROVIDER_LOCK_INVALID")
				}
				it := v.ElementIterator()
				for it.Next() {
					_, x := it.Element()
					if x.Type() != cty.String || x.IsNull() || !x.IsKnown() {
						return runs.Fail("PROVIDER_LOCK_INVALID")
					}
					hash := x.AsString()
					if !strings.HasPrefix(hash, "h1:") && !strings.HasPrefix(hash, "zh:") {
						return runs.Fail("PROVIDER_LOCK_INVALID")
					}
					p.Hashes = append(p.Hashes, hash)
				}
			default:
				return runs.Fail("PROVIDER_LOCK_INVALID")
			}
		}
		sort.Strings(p.Hashes)
		providers = append(providers, p)
	}
	sort.Slice(providers, func(i, j int) bool { return providers[i].Address < providers[j].Address })
	if runs.Digest(providers) != runs.Digest(m.Providers) {
		return runs.Fail("PROVIDER_LOCK_INVALID")
	}
	return nil
}
