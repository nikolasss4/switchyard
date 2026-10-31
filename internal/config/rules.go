package config

import "net/http"

// Matches reports whether every condition of the rule holds. Empty conditions
// match anything.
func (r *Rule) Matches(model, tenant string, h http.Header) bool {
	if r.When.Tenant != "" && r.When.Tenant != tenant {
		return false
	}
	if r.When.Model != "" && r.When.Model != model {
		return false
	}
	for k, v := range r.When.Headers {
		if h.Get(k) != v {
			return false
		}
	}
	return true
}
