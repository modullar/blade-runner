package egress

// sharedSuffixes are domains under which unrelated parties register names: ICANN registry
// suffixes with more than one label (co.uk) and "private" multi-tenant suffixes where anyone can
// get a subdomain (github.io, herokuapp.com). A wildcard over one of them, `*.github.io`, would
// allow every tenant's site, so ParseAllowlist refuses it. A name BELOW one (`*.me.github.io`,
// `*.example.co.uk`) belongs to one registrant and is fine, and so is an exact entry.
//
// This list is small and embedded on purpose (the proxy image carries no data files and does
// no network lookups) and it is NOT exhaustive: the Public Suffix List has thousands of entries.
// It catches the common, dangerous ones; the operator remains responsible for every wildcard
// they write, and should treat a wildcard as trusting everything its owner can create. Add to
// it when one is missed.
var sharedSuffixes = map[string]bool{}

func init() {
	for _, s := range []string{
		// ICANN, multi-label
		"co.uk", "org.uk", "me.uk", "ltd.uk", "plc.uk", "net.uk", "ac.uk", "gov.uk", "sch.uk",
		"com.au", "net.au", "org.au", "edu.au", "gov.au", "id.au",
		"co.nz", "net.nz", "org.nz", "govt.nz",
		"co.jp", "ne.jp", "or.jp", "ac.jp", "go.jp",
		"co.in", "net.in", "org.in", "firm.in", "gen.in",
		"co.za", "org.za", "net.za", "gov.za",
		"com.br", "net.br", "org.br", "gov.br",
		"com.cn", "net.cn", "org.cn", "gov.cn",
		"com.mx", "org.mx", "gob.mx",
		"com.tr", "org.tr", "com.sg", "com.hk", "com.tw", "com.ar", "com.co", "co.kr", "or.kr", "co.il", "co.id",
		// private, multi-tenant
		"github.io", "githubusercontent.com", "gitlab.io", "herokuapp.com", "herokussl.com",
		"amazonaws.com", "s3.amazonaws.com", "cloudfront.net", "elasticbeanstalk.com", "awsapprunner.com",
		"azurewebsites.net", "azureedge.net", "blob.core.windows.net", "cloudapp.azure.com", "trafficmanager.net",
		"appspot.com", "web.app", "firebaseapp.com", "run.app", "cloudfunctions.net", "storage.googleapis.com",
		"vercel.app", "now.sh", "netlify.app", "pages.dev", "workers.dev", "fly.dev", "onrender.com", "railway.app",
		"ngrok.io", "ngrok-free.app", "trycloudflare.com", "glitch.me", "repl.co", "replit.app",
		"blogspot.com", "wordpress.com", "myshopify.com", "wixsite.com", "weebly.com", "surge.sh",
		"readthedocs.io", "pythonanywhere.com", "000webhostapp.com", "dyndns.org", "no-ip.org", "duckdns.org",
	} {
		sharedSuffixes[s] = true
	}
}

// isSharedSuffix reports whether name is itself one of the listed suffixes.
func isSharedSuffix(name string) bool { return sharedSuffixes[name] }
