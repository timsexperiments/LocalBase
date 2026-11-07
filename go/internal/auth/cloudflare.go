package auth

// CloudflareLogout is where a Cloudflare Access session signs out.
func (b *Browser) CloudflareLogout() string {
	if b.Enabled() && b.Config.Provider.Kind == "cloudflare-access" {
		return "/cdn-cgi/access/logout"
	}
	return ""
}
