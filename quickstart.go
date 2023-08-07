package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	_ "embed"

	"golang.org/x/crypto/bcrypt"

	"github.com/mjl-/sconf"

	"github.com/mjl-/mox/config"
	"github.com/mjl-/mox/dns"
	"github.com/mjl-/mox/dnsbl"
	"github.com/mjl-/mox/mox-"
	"github.com/mjl-/mox/smtp"
	"github.com/mjl-/mox/store"
)

//go:embed mox.service
var moxService string

func pwgen() string {
	rand := mox.NewRand()
	chars := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789!@#$%^&*-_;:,<.>/"
	s := ""
	for i := 0; i < 12; i++ {
		s += string(chars[rand.Intn(len(chars))])
	}
	return s
}

func cmdQuickstart(c *cmd) {
	c.params = "[-existing-webserver] [-hostname host] user@domain [user | uid]"
	c.help = `Quickstart generates configuration files and prints instructions to quickly set up a mox instance.

Quickstart writes configuration files, prints initial admin and account
passwords, DNS records you should create. If you run it on Linux it writes a
systemd service file and prints commands to enable and start mox as service.

The user or uid is optional, defaults to "mox", and is the user or uid/gid mox
will run as after initialization.

Quickstart assumes mox will run on the machine you run quickstart on and uses
its host name and public IPs. On many systems the hostname is not a fully
qualified domain name, but only the first dns "label", e.g. "mail" in case of
"mail.example.org". If so, quickstart does a reverse DNS lookup to find the
hostname, and as fallback uses the label plus the domain of the email address
you specified. Use flag -hostname to explicitly specify the hostname mox will
run on.

Mox is by far easiest to operate if you let it listen on port 443 (HTTPS) and
80 (HTTP). TLS will be fully automatic with ACME with Let's Encrypt.

You can run mox along with an existing webserver, but because of MTA-STS and
autoconfig, you'll need to forward HTTPS traffic for two domains to mox. Run
"mox quickstart -existing-webserver ..." to generate configuration files and
instructions for configuring mox along with an existing webserver.

But please first consider configuring mox on port 443. It can itself serve
domains with HTTP/HTTPS, including with automatic TLS with ACME, is easily
configured through both configuration files and admin web interface, and can act
as a reverse proxy (and static file server for that matter), so you can forward
traffic to your existing backend applications. Look for "WebHandlers:" in the
output of "mox config describe-domains" and see the output of "mox example
webhandlers".
`
	var existingWebserver bool
	var hostname string
	c.flag.BoolVar(&existingWebserver, "existing-webserver", false, "use if a webserver is already running, so mox won't listen on port 80 and 443; you'll have to provide tls certificates/keys, and configure the existing webserver as reverse proxy, forwarding requests to mox.")
	c.flag.StringVar(&hostname, "hostname", "", "hostname mox will run on, by default the hostname of the machine quickstart runs on; if specified, the IPs for the hostname are configured for the public listener")
	args := c.Parse()
	if len(args) != 1 && len(args) != 2 {
		c.Usage()
	}

	// We take care to cleanup created files when we error out.
	// We don't want to get a new user into trouble with half of the files
	// after encountering an error.

	// We use fatalf instead of log.Fatal* to cleanup files.
	var cleanupPaths []string
	fatalf := func(format string, args ...any) {
		// We remove in reverse order because dirs would have been created first and must
		// be removed last, after their files have been removed.
		for i := len(cleanupPaths) - 1; i >= 0; i-- {
			p := cleanupPaths[i]
			if err := os.Remove(p); err != nil {
				log.Printf("cleaning up %q: %s", p, err)
			}
		}

		log.Fatalf(format, args...)
	}

	xwritefile := func(path string, data []byte, perm os.FileMode) {
		os.MkdirAll(filepath.Dir(path), 0770)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			fatalf("creating file %q: %s", path, err)
		}
		cleanupPaths = append(cleanupPaths, path)
		_, err = f.Write(data)
		if err == nil {
			err = f.Close()
		}
		if err != nil {
			fatalf("writing file %q: %s", path, err)
		}
	}

	addr, err := smtp.ParseAddress(args[0])
	if err != nil {
		fatalf("parsing email address: %s", err)
	}
	accountName := addr.Localpart.String()
	domain := addr.Domain

	for _, c := range accountName {
		if c > 0x7f {
			fmt.Printf(`NOTE: Username %q is not ASCII-only. It is recommended you also configure an
ASCII-only alias. Both for delivery of email from other systems, and for
logging in with IMAP.

`, accountName)
			break
		}
	}

	resolver := dns.StrictResolver{}
	// We don't want to spend too much total time on the DNS lookups. Because DNS may
	// not work during quickstart, and we don't want to loop doing requests and having
	// to wait for a timeout each time.
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer resolveCancel()

	// We are going to find the (public) IPs to listen on and possibly the host name.

	// Start with reasonable defaults. We'll replace them specific IPs, if we can find them.
	publicListenerIPs := []string{"0.0.0.0", "::"}
	privateListenerIPs := []string{"127.0.0.1", "::1"}

	// If we find IPs based on network interfaces, {public,private}ListenerIPs are set
	// based on these values.
	var privateIPs, publicIPs []string

	var dnshostname dns.Domain
	if hostname == "" {
		// Gather IP addresses for public and private listeners.
		// If we cannot find addresses for a category we fallback to all ips or localhost ips.
		// We look at each network interface. If an interface has a private address, we
		// conservatively assume all addresses on that interface are private.
		ifaces, err := net.Interfaces()
		if err != nil {
			fatalf("listing network interfaces: %s", err)
		}
		parseAddrIP := func(s string) net.IP {
			if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
				s = s[1 : len(s)-1]
			}
			ip, _, _ := net.ParseCIDR(s)
			return ip
		}
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 {
				continue
			}
			addrs, err := iface.Addrs()
			if err != nil {
				fatalf("listing address for network interface: %s", err)
			}
			if len(addrs) == 0 {
				continue
			}

			// todo: should we detect temporary/ephemeral ipv6 addresses and not add them?
			var nonpublic bool
			for _, addr := range addrs {
				ip := parseAddrIP(addr.String())
				if ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
					continue
				}
				if ip.IsLoopback() || ip.IsPrivate() {
					nonpublic = true
					break
				}
			}

			for _, addr := range addrs {
				ip := parseAddrIP(addr.String())
				if ip == nil {
					continue
				}
				if ip.IsInterfaceLocalMulticast() || ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() || ip.IsMulticast() {
					continue
				}
				if nonpublic {
					privateIPs = append(privateIPs, ip.String())
				} else {
					publicIPs = append(publicIPs, ip.String())
				}
			}
		}

		if len(publicIPs) > 0 {
			publicListenerIPs = publicIPs
		}
		if len(privateIPs) > 0 {
			privateListenerIPs = privateIPs
		}

		hostnameStr, err := os.Hostname()
		if err != nil {
			fatalf("hostname: %s", err)
		}
		if strings.Contains(hostnameStr, ".") {
			dnshostname, err = dns.ParseDomain(hostnameStr)
			if err != nil {
				fatalf("parsing hostname: %v", err)
			}
		} else {
			// It seems Linux machines don't have a single FQDN configured. E.g. /etc/hostname
			// is just the name without domain. We'll look up the names for all IPs, and hope
			// to find a single FQDN name (with at least 1 dot).
			names := map[string]struct{}{}
			if len(publicIPs) > 0 {
				fmt.Printf("Trying to find hostname by reverse lookup of public IPs %s...", strings.Join(publicIPs, ", "))
			}
			var warned bool
			warnf := func(format string, args ...any) {
				warned = true
				fmt.Printf("\n%s", fmt.Sprintf(format, args...))
			}
			for _, ip := range publicIPs {
				revctx, revcancel := context.WithTimeout(resolveCtx, 5*time.Second)
				defer revcancel()
				l, err := resolver.LookupAddr(revctx, ip)
				if err != nil {
					warnf("WARNING: looking up reverse name(s) for %s: %v", ip, err)
				}
				for _, name := range l {
					if strings.Contains(name, ".") {
						names[name] = struct{}{}
					}
				}
			}
			var nameList []string
			for k := range names {
				nameList = append(nameList, strings.TrimRight(k, "."))
			}
			sort.Slice(nameList, func(i, j int) bool {
				return nameList[i] < nameList[j]
			})
			if len(nameList) == 0 {
				dnshostname, err = dns.ParseDomain(hostnameStr + "." + domain.Name())
				if err != nil {
					fmt.Println()
					fatalf("parsing hostname: %v", err)
				}
				warnf(`WARNING: cannot determine hostname because the system name is not an FQDN and
no public IPs resolving to an FQDN were found. Quickstart guessed the host name
below. If it is not correct, please remove the generated config files and run
quickstart again with the -hostname flag.

		%s
`, dnshostname)
			} else {
				if len(nameList) > 1 {
					warnf(`WARNING: multiple hostnames found for the public IPs, using the first of: %s
If this is not correct, remove the generated config files and run quickstart
again with the -hostname flag.
`, strings.Join(nameList, ", "))
				}
				dnshostname, err = dns.ParseDomain(nameList[0])
				if err != nil {
					fmt.Println()
					fatalf("parsing hostname %s: %v", nameList[0], err)
				}
			}
			if warned {
				fmt.Printf("\n\n")
			} else {
				fmt.Printf(" found %s\n", dnshostname)
			}
		}
	} else {
		// Host name was explicitly configured on command-line. We'll try to use its public
		// IPs below.
		var err error
		dnshostname, err = dns.ParseDomain(hostname)
		if err != nil {
			fatalf("parsing hostname: %v", err)
		}
	}

	fmt.Printf("Looking up IPs for hostname %s...", dnshostname)
	ipctx, ipcancel := context.WithTimeout(resolveCtx, 5*time.Second)
	defer ipcancel()
	ips, err := resolver.LookupIPAddr(ipctx, dnshostname.ASCII+".")
	ipcancel()
	var xips []net.IPAddr
	var xipstrs []string
	var dnswarned bool
	for _, ip := range ips {
		// During linux install, you may get an alias for you full hostname in /etc/hosts
		// resolving to 127.0.1.1, which would result in a false positive about the
		// hostname having a record. Filter it out. It is a bit surprising that hosts don't
		// otherwise know their FQDN.
		if ip.IP.IsLoopback() {
			dnswarned = true
			fmt.Printf("\n\nWARNING: Your hostname is resolving to a loopback IP address %s. This likely breaks email delivery to local accounts. /etc/hosts likely contains a line like %q. Either replace it with your actual IP(s), or remove the line.\n", ip.IP, fmt.Sprintf("%s %s", ip.IP, dnshostname.ASCII))
			continue
		}
		xips = append(xips, ip)
		xipstrs = append(xipstrs, ip.String())
	}
	if err == nil && len(xips) == 0 {
		// todo: possibly check this by trying to resolve without using /etc/hosts?
		err = errors.New("hostname not in dns, probably only in /etc/hosts")
	}
	ips = xips
	if hostname != "" {
		// Host name was specified, assume we will run on a machine with those IPs.
		publicListenerIPs = xipstrs
		publicIPs = xipstrs
	}
	if err != nil {
		if !dnswarned {
			fmt.Printf("\n")
		}
		dnswarned = true
		fmt.Printf(`
WARNING: Quickstart assumed the hostname of this machine is %s and generates a
config for that host, but could not retrieve that name from DNS:

	%s

This likely means one of two things:

1. You don't have any DNS records for this machine at all. You should add them
   before continuing.
2. The hostname mentioned is not the correct host name of this machine. You will
   have to replace the hostname in the suggested DNS records and generated
   config/mox.conf file. Make sure your hostname resolves to your public IPs, and
   your public IPs resolve back (reverse) to your hostname.


`, dnshostname, err)
	}

	if !dnswarned {
		fmt.Printf(" OK\n")

		var l []string
		type result struct {
			IP    string
			Addrs []string
			Err   error
		}
		results := make(chan result)
		for _, ip := range ips {
			s := ip.String()
			l = append(l, s)
			go func() {
				revctx, revcancel := context.WithTimeout(resolveCtx, 5*time.Second)
				defer revcancel()
				addrs, err := resolver.LookupAddr(revctx, s)
				results <- result{s, addrs, err}
			}()
		}
		fmt.Printf("Looking up reverse names for IP(s) %s...", strings.Join(l, ", "))
		var warned bool
		warnf := func(format string, args ...any) {
			fmt.Printf("\nWARNING: %s", fmt.Sprintf(format, args...))
			warned = true
		}
		for i := 0; i < len(ips); i++ {
			r := <-results
			if r.Err != nil {
				warnf("looking up reverse name for %s: %v", r.IP, r.Err)
				continue
			}
			if len(r.Addrs) != 1 {
				warnf("expected exactly 1 name for %s, got %d (%v)", r.IP, len(r.Addrs), r.Addrs)
			}
			var match bool
			for i, a := range r.Addrs {
				a = strings.TrimRight(a, ".")
				r.Addrs[i] = a // For potential error message below.
				d, err := dns.ParseDomain(a)
				if err != nil {
					warnf("parsing reverse name %q for %s: %v", a, r.IP, err)
				}
				if d == dnshostname {
					match = true
				}
			}
			if !match {
				warnf("reverse name(s) %s for ip %s do not match hostname %s, which will cause other mail servers to reject incoming messages from this IP", strings.Join(r.Addrs, ","), r.IP, dnshostname)
			}
		}
		if warned {
			fmt.Printf("\n\n")
		} else {
			fmt.Printf(" OK\n")
		}
	}

	zones := []dns.Domain{
		{ASCII: "sbl.spamhaus.org"},
		{ASCII: "bl.spamcop.net"},
	}
	if len(publicIPs) > 0 {
		fmt.Printf("Checking whether your public IPs are listed in popular DNS block lists...")
		var listed bool
		for _, zone := range zones {
			for _, ip := range publicIPs {
				dnsblctx, dnsblcancel := context.WithTimeout(resolveCtx, 5*time.Second)
				status, expl, err := dnsbl.Lookup(dnsblctx, resolver, zone, net.ParseIP(ip))
				dnsblcancel()
				if status == dnsbl.StatusPass {
					continue
				}
				errstr := ""
				if err != nil {
					errstr = fmt.Sprintf(" (%s)", err)
				}
				fmt.Printf("\nWARNING: checking your public IP %s in DNS block list %s: %v %s%s", ip, zone.Name(), status, expl, errstr)
				listed = true
			}
		}
		if listed {
			log.Printf(`
Other mail servers are likely to reject email from IPs that are in a blocklist.
If all your IPs are in block lists, you will encounter problems delivering
email. Your IP may be in block lists only temporarily. To see if your IPs are
listed in more DNS block lists, visit:

`)
			for _, ip := range publicIPs {
				fmt.Printf("- https://multirbl.valli.org/lookup/%s.html\n", url.PathEscape(ip))
			}
			fmt.Printf("\n")
		} else {
			fmt.Printf(" OK\n")
		}
	}
	fmt.Printf("\n")

	user := "mox"
	if len(args) == 2 {
		user = args[1]
	}

	dc := config.Dynamic{}
	sc := config.Static{
		DataDir:           "../data",
		User:              user,
		LogLevel:          "debug", // Help new users, they'll bring it back to info when it all works.
		Hostname:          dnshostname.Name(),
		AdminPasswordFile: "adminpasswd",
	}
	if !existingWebserver {
		sc.ACME = map[string]config.ACME{
			"letsencrypt": {
				DirectoryURL: "https://acme-v02.api.letsencrypt.org/directory",
				ContactEmail: args[0], // todo: let user specify an alternative fallback address?
			},
		}
	}
	dataDir := "data" // ../data is relative to config/
	os.MkdirAll(dataDir, 0770)
	adminpw := pwgen()
	adminpwhash, err := bcrypt.GenerateFromPassword([]byte(adminpw), bcrypt.DefaultCost)
	if err != nil {
		fatalf("generating hash for generated admin password: %s", err)
	}
	xwritefile(filepath.Join("config", sc.AdminPasswordFile), adminpwhash, 0660)
	fmt.Printf("Admin password: %s\n", adminpw)

	public := config.Listener{
		IPs: publicListenerIPs,
	}
	public.SMTP.Enabled = true
	public.Submissions.Enabled = true
	public.IMAPS.Enabled = true

	if existingWebserver {
		hostbase := fmt.Sprintf("path/to/%s", dnshostname.Name())
		mtastsbase := fmt.Sprintf("path/to/mta-sts.%s", domain.Name())
		autoconfigbase := fmt.Sprintf("path/to/autoconfig.%s", domain.Name())
		public.TLS = &config.TLS{
			KeyCerts: []config.KeyCert{
				{CertFile: hostbase + "-chain.crt.pem", KeyFile: hostbase + ".key.pem"},
				{CertFile: mtastsbase + "-chain.crt.pem", KeyFile: mtastsbase + ".key.pem"},
				{CertFile: autoconfigbase + "-chain.crt.pem", KeyFile: autoconfigbase + ".key.pem"},
			},
		}
	} else {
		public.TLS = &config.TLS{
			ACME: "letsencrypt",
		}
		public.AutoconfigHTTPS.Enabled = true
		public.MTASTSHTTPS.Enabled = true
		public.WebserverHTTP.Enabled = true
		public.WebserverHTTPS.Enabled = true
	}

	// Suggest blocklists, but we'll comment them out after generating the config.
	for _, zone := range zones {
		public.SMTP.DNSBLs = append(public.SMTP.DNSBLs, zone.Name())
	}

	internal := config.Listener{
		IPs:      privateListenerIPs,
		Hostname: "localhost",
	}
	internal.AccountHTTP.Enabled = true
	internal.AdminHTTP.Enabled = true
	internal.MetricsHTTP.Enabled = true
	internal.WebmailHTTP.Enabled = true
	if existingWebserver {
		internal.AccountHTTP.Port = 1080
		internal.AdminHTTP.Port = 1080
		internal.WebmailHTTP.Port = 1080
		internal.AutoconfigHTTPS.Enabled = true
		internal.AutoconfigHTTPS.Port = 81
		internal.AutoconfigHTTPS.NonTLS = true
		internal.MTASTSHTTPS.Enabled = true
		internal.MTASTSHTTPS.Port = 81
		internal.MTASTSHTTPS.NonTLS = true
		internal.WebserverHTTP.Enabled = true
		internal.WebserverHTTP.Port = 81
	}

	sc.Listeners = map[string]config.Listener{
		"public":   public,
		"internal": internal,
	}
	sc.Postmaster.Account = accountName
	sc.Postmaster.Mailbox = "Postmaster"

	mox.ConfigStaticPath = "config/mox.conf"
	mox.ConfigDynamicPath = "config/domains.conf"

	mox.Conf.DynamicLastCheck = time.Now() // Prevent error logging by Make calls below.

	accountConf := mox.MakeAccountConfig(addr)
	const withMTASTS = true
	confDomain, keyPaths, err := mox.MakeDomainConfig(context.Background(), domain, dnshostname, accountName, withMTASTS)
	if err != nil {
		fatalf("making domain config: %s", err)
	}
	cleanupPaths = append(cleanupPaths, keyPaths...)

	dc.Domains = map[string]config.Domain{
		domain.Name(): confDomain,
	}
	dc.Accounts = map[string]config.Account{
		accountName: accountConf,
	}

	// Build config in memory, so we can easily comment out the DNSBLs config.
	var sb strings.Builder
	sc.CheckUpdates = true // Commented out below.
	if err := sconf.WriteDocs(&sb, &sc); err != nil {
		fatalf("generating static config: %v", err)
	}
	confstr := sb.String()
	confstr = strings.ReplaceAll(confstr, "\nCheckUpdates: true\n", "\n#\n# RECOMMENDED: please enable to stay up to date\n#\n#CheckUpdates: true\n")
	confstr = strings.ReplaceAll(confstr, "DNSBLs:\n", "#DNSBLs:\n")
	for _, bl := range public.SMTP.DNSBLs {
		confstr = strings.ReplaceAll(confstr, "- "+bl+"\n", "#- "+bl+"\n")
	}
	xwritefile("config/mox.conf", []byte(confstr), 0660)

	// Generate domains config, and add a commented out example for delivery to a mailing list.
	var db bytes.Buffer
	if err := sconf.WriteDocs(&db, &dc); err != nil {
		fatalf("generating domains config: %v", err)
	}

	// This approach is a bit horrible, but it generates a convenient
	// example that includes the comments. Though it is gone by the first
	// write of the file by mox.
	odests := fmt.Sprintf("\t\tDestinations:\n\t\t\t%s: nil\n", addr.String())
	var destsExample = struct {
		Destinations map[string]config.Destination
	}{
		Destinations: map[string]config.Destination{
			addr.String(): {
				Rulesets: []config.Ruleset{
					{
						VerifiedDomain: "list.example.org",
						HeadersRegexp: map[string]string{
							"^list-id$": `<name\.list\.example\.org>`,
						},
						ListAllowDomain: "list.example.org",
						Mailbox:         "Lists/Example",
					},
				},
			},
		},
	}
	var destBuf strings.Builder
	if err := sconf.Describe(&destBuf, destsExample); err != nil {
		fatalf("describing destination example: %v", err)
	}
	ndests := odests + "#\t\t\tIf you receive email from mailing lists, you probably want to configure them like the example below.\n"
	for _, line := range strings.Split(destBuf.String(), "\n")[1:] {
		ndests += "#\t\t" + line + "\n"
	}
	dconfstr := strings.ReplaceAll(db.String(), odests, ndests)
	xwritefile("config/domains.conf", []byte(dconfstr), 0660)

	// Verify config.
	skipCheckTLSKeyCerts := existingWebserver
	mc, errs := mox.ParseConfig(context.Background(), "config/mox.conf", true, skipCheckTLSKeyCerts, false)
	if len(errs) > 0 {
		if len(errs) > 1 {
			log.Printf("checking generated config, multiple errors:")
			for _, err := range errs {
				log.Println(err)
			}
			fatalf("aborting due to multiple config errors")
		}
		fatalf("checking generated config: %s", errs[0])
	}
	mox.SetConfig(mc)
	// NOTE: Now that we've prepared the config, we can open the account
	// and set a passsword, and the public key for the DKIM private keys
	// are available for generating the DKIM DNS records below.

	confDomain, ok := mc.Domain(domain)
	if !ok {
		fatalf("cannot find domain in new config")
	}

	acc, _, err := store.OpenEmail(args[0])
	if err != nil {
		fatalf("open account: %s", err)
	}
	cleanupPaths = append(cleanupPaths, dataDir, filepath.Join(dataDir, "accounts"), filepath.Join(dataDir, "accounts", accountName), filepath.Join(dataDir, "accounts", accountName, "index.db"))

	password := pwgen()
	if err := acc.SetPassword(password); err != nil {
		fatalf("setting password: %s", err)
	}
	if err := acc.Close(); err != nil {
		fatalf("closing account: %s", err)
	}
	fmt.Printf("IMAP, SMTP submission and HTTP account password for %s: %s\n\n", args[0], password)
	fmt.Printf(`When configuring your email client, use the email address as username. If
autoconfig/autodiscover does not work, use these settings:
`)
	printClientConfig(domain)

	if existingWebserver {
		fmt.Printf(`
Configuration files have been written to config/mox.conf and
config/domains.conf.

Create the DNS records below. The admin interface can show these same records, and
has a page to check they have been configured correctly.

You must configure your existing webserver to forward requests for:

	https://mta-sts.%s/
	https://autoconfig.%s/

To mox, at:

	http://127.0.0.1:81

If it makes it easier to get a TLS certificate for %s, you can add a
reverse proxy for that hostname too.

You must edit mox.conf and configure the paths to the TLS certificates and keys.
The paths are relative to config/ directory that holds mox.conf! To test if your
config is valid, run:

	./mox config test
`, domain.ASCII, domain.ASCII, dnshostname.ASCII)
	} else {
		fmt.Printf(`
Configuration files have been written to config/mox.conf and
config/domains.conf. You should review them. Then create the DNS records below.
You can also skip creating the DNS records and start mox immediately. The admin
interface can show these same records, and has a page to check they have been
configured correctly.
`)
	}

	// We do not verify the records exist: If they don't exist, we would only be
	// priming dns caches with negative/absent records, causing our "quick setup" to
	// appear to fail or take longer than "quick".

	records, err := mox.DomainRecords(confDomain, domain)
	if err != nil {
		fatalf("making required DNS records")
	}
	fmt.Print("\n\n" + strings.Join(records, "\n") + "\n\n\n\n")

	fmt.Printf(`WARNING: The configuration and DNS records above assume you do not currently
have email configured for your domain. If you do already have email configured,
or if you are sending email for your domain from other machines/services, you
should understand the consequences of the DNS records above before
continuing!
`)
	if os.Getenv("MOX_DOCKER") == "" {
		fmt.Printf(`
You can now start mox with "./mox serve", as root.
`)
	} else {
		fmt.Printf(`
You can now start the mox container.
`)
	}
	fmt.Printf(`
File ownership and permissions are automatically set correctly by mox when
starting up. On linux, you may want to enable mox as a systemd service.

`)

	// For now, we only give service config instructions for linux when not running in docker.
	if runtime.GOOS == "linux" && os.Getenv("MOX_DOCKER") == "" {
		pwd, err := os.Getwd()
		if err != nil {
			log.Printf("current working directory: %v", err)
			pwd = "/home/mox"
		}
		service := strings.ReplaceAll(moxService, "/home/mox", pwd)
		xwritefile("mox.service", []byte(service), 0644)
		cleanupPaths = append(cleanupPaths, "mox.service")
		fmt.Printf(`See mox.service for a systemd service file. To enable and start:

	sudo chmod 644 mox.service
	sudo systemctl enable $PWD/mox.service
	sudo systemctl start mox.service
	sudo journalctl -f -u mox.service # See logs
`)
	}

	fmt.Printf(`
After starting mox, the web interfaces are served at:

http://localhost/         - account (email address as username)
http://localhost/webmail/ - webmail (email address as username)
http://localhost/admin/   - admin (empty username)

To access these from your browser, run
"ssh -L 8080:localhost:80 you@yourmachine" locally and open
http://localhost:8080/[...].

For secure email exchange you should have a strictly validating DNSSEC
resolver. An easy and the recommended way is to install unbound.

If you run into problem, have questions/feedback or found a bug, please let us
know. Mox needs your help!

Enjoy!
`)

	if !existingWebserver {
		fmt.Printf(`
PS: If you want to run mox along side an existing webserver that uses port 443
and 80, see "mox help quickstart" with the -existing-webserver option.
`)
	}

	cleanupPaths = nil
}
