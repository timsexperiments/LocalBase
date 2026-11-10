package gatewaytest

// AccessRoute is the access management API.
const AccessRoute = "/_localbase/access-management"

// ChatRole is a browser access role that may only chat.
const ChatRole = `{"name":"chat","description":"","permissions":["inference:chat"]}`

// AdminRole is a browser access role that reads and manages access.
const AdminRole = `{"name":"admin","description":"","permissions":["access:read","access:manage"]}`

// ConfigureCloudflare is an access request that makes Cloudflare Access the browser provider.
const ConfigureCloudflare = `{"action":"configure-cloudflare","provider":{"kind":"cloudflare-access","teamDomain":"team.cloudflareaccess.com","audience":"aud"},"origin":"https://lb.example.test","permissions":["inference:chat"]}`

// PolicyRequest is an access request applying a policy of these roles, bindings and default role
// (JSON), expecting revision (a JSON string, or null for none yet).
func PolicyRequest(revision, roles, bindings, defaultRole string) string {
	return `{"action":"apply-policy","expectedPolicyRevision":` + revision + `,"policy":{"roles":[` + roles + `],"bindings":[` + bindings + `],"defaultRole":` + defaultRole + `}}`
}
