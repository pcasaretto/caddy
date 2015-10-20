// Package letsencrypt integrates Let's Encrypt functionality into Caddy
// with first-class support for creating and renewing certificates
// automatically. It is designed to configure sites for HTTPS by default.
package letsencrypt

import (
	"encoding/json"
	"errors"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/mholt/caddy/middleware"
	"github.com/mholt/caddy/middleware/redirect"
	"github.com/mholt/caddy/server"
	"github.com/xenolf/lego/acme"
)

// Activate sets up TLS for each server config in configs
// as needed. It only skips the config if the cert and key
// are already provided or if plaintext http is explicitly
// specified as the port.
//
// This function may prompt the user to provide an email
// address if none is available through other means. It
// prefers the email address specified in the config, but
// if that is not available it will check the command line
// argument. If absent, it will use the most recent email
// address from last time. If there isn't one, the user
// will be prompted. If the user leaves email blank, <TODO>.
func Activate(configs []server.Config) ([]server.Config, error) {
	// First identify and configure any elligible hosts for which
	// we already have certs and keys in storage from last time.
	configLen := len(configs) // avoid infinite loop since this loop appends to the slice
	for i := 0; i < configLen; i++ {
		if existingCertAndKey(configs[i].Host) && configs[i].TLS.LetsEncryptEmail != "off" {
			configs = autoConfigure(&configs[i], configs)
		}
	}
	// Handle cert renewal on Startup
	processCertificateRenewal(configs)

	// Group configs by LE email address; this will help us
	// reduce round-trips when getting the certs.
	initMap, err := groupConfigsByEmail(configs)
	if err != nil {
		return configs, err
	}

	// Loop through each email address and obtain certs; we can obtain more
	// than one certificate per email address, and still save them individually.
	for leEmail, serverConfigs := range initMap {
		// make client to service this email address with CA server
		client, err := newClient(leEmail)
		if err != nil {
			return configs, err
		}

		// client is ready, so let's get free, trusted SSL certificates! yeah!
		certificates, err := obtainCertificates(client, serverConfigs)
		if err != nil {
			return configs, err
		}

		// ... that's it. save the certs, keys, and metadata files to disk
		err = saveCertsAndKeys(certificates)
		if err != nil {
			return configs, err
		}

		// it all comes down to this: turning TLS on for all the configs
		for _, cfg := range serverConfigs {
			configs = autoConfigure(cfg, configs)
		}
	}

	go renewalFunc(configs)

	return configs, nil
}

// groupConfigsByEmail groups configs by the Let's Encrypt email address
// associated to them or to the default Let's Encrypt email address. If the
// default email is not available, the user will be prompted to provide one.
//
// This function also filters out configs that don't need extra TLS help.
// Configurations with a manual TLS configuration or one that is already
// found in storage will not be added to any group.
func groupConfigsByEmail(configs []server.Config) (map[string][]*server.Config, error) {
	// configQualifies returns true if cfg qualifes for automatic LE activation
	configQualifies := func(cfg server.Config) bool {
		return cfg.TLS.Certificate == "" && // user could provide their own cert and key
			cfg.TLS.Key == "" &&

			// user can force-disable automatic HTTPS for this host
			cfg.Port != "http" &&
			cfg.TLS.LetsEncryptEmail != "off" &&

			// obviously we get can't certs for loopback or internal hosts
			cfg.Host != "localhost" &&
			cfg.Host != "" &&
			cfg.Host != "0.0.0.0" &&
			cfg.Host != "::1" &&
			!strings.HasPrefix(cfg.Host, "127.") &&
			!strings.HasPrefix(cfg.Host, "10.") &&

			// make sure an HTTPS version of this config doesn't exist in the list already
			!hostHasOtherScheme(cfg.Host, "https", configs)
	}

	initMap := make(map[string][]*server.Config)
	for i := 0; i < len(configs); i++ {
		if !configQualifies(configs[i]) {
			continue
		}
		leEmail := getEmail(configs[i])
		if leEmail == "" {
			return nil, errors.New("must have email address to serve HTTPS without existing certificate and key")
		}
		initMap[leEmail] = append(initMap[leEmail], &configs[i])
	}
	return initMap, nil
}

// existingCertAndKey returns true if the host has a certificate
// and private key in storage already, false otherwise.
func existingCertAndKey(host string) bool {
	_, err := os.Stat(storage.SiteCertFile(host))
	if err != nil {
		return false
	}
	_, err = os.Stat(storage.SiteKeyFile(host))
	if err != nil {
		return false
	}
	return true
}

// newClient creates a new ACME client to facilitate communication
// with the Let's Encrypt CA server on behalf of the user specified
// by leEmail. As part of this process, a user will be loaded from
// disk (if already exists) or created new and registered via ACME
// and saved to the file system for next time.
func newClient(leEmail string) (*acme.Client, error) {
	// Look up or create the LE user account
	leUser, err := getUser(leEmail)
	if err != nil {
		return nil, err
	}

	// The client facilitates our communication with the CA server.
	client := acme.NewClient(caURL, &leUser, rsaKeySizeToUse, exposePort, true) // TODO: Dev mode is enabled

	// If not registered, the user must register an account with the CA
	// and agree to terms
	if leUser.Registration == nil {
		reg, err := client.Register()
		if err != nil {
			return nil, errors.New("registration error: " + err.Error())
		}
		leUser.Registration = reg

		// TODO: we can just do the agreement once: when registering, right?
		err = client.AgreeToTos()
		if err != nil {
			saveUser(leUser) // TODO: Might as well try, right? Error check?
			return nil, errors.New("error agreeing to terms: " + err.Error())
		}

		// save user to the file system
		err = saveUser(leUser)
		if err != nil {
			return nil, errors.New("could not save user: " + err.Error())
		}
	}

	return client, nil
}

// obtainCertificates obtains certificates from the CA server for
// the configurations in serverConfigs using client.
func obtainCertificates(client *acme.Client, serverConfigs []*server.Config) ([]acme.CertificateResource, error) {
	// collect all the hostnames into one slice
	var hosts []string
	for _, cfg := range serverConfigs {
		hosts = append(hosts, cfg.Host)
	}

	certificates, err := client.ObtainCertificates(hosts)
	if err != nil {
		return nil, errors.New("error obtaining certs: " + err.Error())
	}

	return certificates, nil
}

// saveCertificates saves each certificate resource to disk. This
// includes the certificate file itself, the private key, and the
// metadata file.
func saveCertsAndKeys(certificates []acme.CertificateResource) error {
	for _, cert := range certificates {
		os.MkdirAll(storage.Site(cert.Domain), 0700)

		// Save cert
		err := ioutil.WriteFile(storage.SiteCertFile(cert.Domain), cert.Certificate, 0600)
		if err != nil {
			return err
		}

		// Save private key
		err = ioutil.WriteFile(storage.SiteKeyFile(cert.Domain), cert.PrivateKey, 0600)
		if err != nil {
			return err
		}

		// Save cert metadata
		jsonBytes, err := json.MarshalIndent(&cert, "", "\t")
		if err != nil {
			return err
		}
		err = ioutil.WriteFile(storage.SiteMetaFile(cert.Domain), jsonBytes, 0600)
		if err != nil {
			return err
		}
	}
	return nil
}

// autoConfigure enables TLS on cfg and appends, if necessary, a new config
// to allConfigs that redirects plaintext HTTP to its new HTTPS counterpart.
func autoConfigure(cfg *server.Config, allConfigs []server.Config) []server.Config {
	cfg.TLS.Certificate = storage.SiteCertFile(cfg.Host)
	cfg.TLS.Key = storage.SiteKeyFile(cfg.Host)
	cfg.TLS.Enabled = true
	cfg.Port = "https"

	// Set up http->https redirect as long as there isn't already
	// a http counterpart in the configs
	if !hostHasOtherScheme(cfg.Host, "http", allConfigs) {
		allConfigs = append(allConfigs, redirPlaintextHost(*cfg))
	}

	return allConfigs
}

// hostHasOtherScheme tells you whether there is another config in the list
// for the same host but with the port equal to scheme. For example, to see
// if example.com has a https variant already, pass in example.com and
// "https" along with the list of configs.
func hostHasOtherScheme(host, scheme string, allConfigs []server.Config) bool {
	for _, otherCfg := range allConfigs {
		if otherCfg.Host == host && otherCfg.Port == scheme {
			return true
		}
	}
	return false
}

// redirPlaintextHost returns a new plaintext HTTP configuration for
// a virtualHost that simply redirects to cfg, which is assumed to
// be the HTTPS configuration. The returned configuration is set
// to listen on the "http" port (port 80).
func redirPlaintextHost(cfg server.Config) server.Config {
	redirMidware := func(next middleware.Handler) middleware.Handler {
		return redirect.Redirect{Next: next, Rules: []redirect.Rule{
			{
				FromScheme: "http",
				FromPath:   "/",
				To:         "https://" + cfg.Host + "{uri}",
				Code:       http.StatusMovedPermanently,
			},
		}}
	}

	return server.Config{
		Host: cfg.Host,
		Port: "http",
		Middleware: map[string][]middleware.Middleware{
			"/": []middleware.Middleware{redirMidware},
		},
	}
}

func renewalFunc(configs []server.Config) {
	nextRun, err := processCertificateRenewal(configs)
	if err != nil {
		log.Printf("[ERROR] Could not start renewal routine. %v", err)
		return
	}

	for {
		timer := time.NewTimer(time.Duration(nextRun) * time.Hour)
		<-timer.C
		nextRun, err = processCertificateRenewal(configs)
		if err != nil {
			log.Printf("[ERROR] Renewal routing stopped. %v", err)
			return
		}
	}
}

// checkCertificateRenewal loops through all configured
// sites and looks for certificates to renew. Nothing is mutated
// through this function. The changes happen directly on disk.
func processCertificateRenewal(configs []server.Config) (int, error) {
	log.Print("[INFO] Processing certificate renewals...")
	// Check if we should run. If not, get out of here.
	next, err := getNextRenewalShedule()
	if err != nil {
		return 0, err
	}

	if next > 0 {
		return next, nil
	}

	// We are executing. Write the current timestamp into the file.
	err = ioutil.WriteFile(storage.RenewTimerFile(), []byte(time.Now().UTC().Format(time.RFC3339)), 0600)
	if err != nil {
		return 0, err
	}
	next = renewTimer

	for _, cfg := range configs {
		// Check if this entry is TLS enabled and managed by LE
		if !cfg.TLS.Enabled || !existingCertAndKey(cfg.Host) {
			continue
		}

		// Read the certificate and get the NotAfter time.
		certBytes, err := ioutil.ReadFile(storage.SiteCertFile(cfg.Host))
		if err != nil {
			return 0, err
		}
		expTime, err := acme.GetPEMCertExpiration(certBytes)
		if err != nil {
			return 0, err
		}

		// The time returned from the certificate is always in UTC.
		// So calculate the time left with local time as UTC.
		// Directly convert it to days for the following checks.
		daysLeft := int(expTime.Sub(time.Now().UTC()).Hours() / 24)

		// Renew on two or less days remaining.
		if daysLeft <= 2 {
			log.Printf("[WARN] There are %d days left on the certificate of %s. Trying to renew now.", daysLeft, cfg.Host)
			client, err := newClient(getEmail(cfg))
			if err != nil {
				return 0, err
			}

			// Read metadata
			metaBytes, err := ioutil.ReadFile(storage.SiteMetaFile(cfg.Host))
			if err != nil {
				return 0, err
			}

			privBytes, err := ioutil.ReadFile(storage.SiteKeyFile(cfg.Host))
			if err != nil {
				return 0, err
			}

			var certMeta acme.CertificateResource
			err = json.Unmarshal(metaBytes, &certMeta)
			certMeta.Certificate = certBytes
			certMeta.PrivateKey = privBytes

			// Renew certificate.
			// TODO: revokeOld should be an option in the caddyfile
			newCertMeta, err := client.RenewCertificate(certMeta, true)
			if err != nil {
				return 0, err
			}

			saveCertsAndKeys([]acme.CertificateResource{newCertMeta})
		}

		// Warn on 14 days remaining
		if daysLeft <= 14 {
			log.Printf("[WARN] There are %d days left on the certificate of %s. Will renew on two days left.\n", daysLeft, cfg.Host)
		}
	}

	return next, nil
}

// getNextRenewalShedule calculates the offset in hours the renew process should
// run from the current time. If the file the time is in does not exists, the
// function returns zero to trigger a renew asap.
func getNextRenewalShedule() (int, error) {

	// Check if the file exists. If it does not, return 0 to indicate immediate processing.
	if _, err := os.Stat(storage.RenewTimerFile()); os.IsNotExist(err) {
		return 0, nil
	}

	renewTimeBytes, err := ioutil.ReadFile(storage.RenewTimerFile())
	if err != nil {
		return 0, err
	}

	renewalTime, err := time.Parse(time.RFC3339, string(renewTimeBytes))
	if err != nil {
		return 0, err
	}

	// The time read from the file was equal or more then 24 hours in the past,
	// write the current time to the file and return true.
	hoursSinceRenew := int(time.Now().UTC().Sub(renewalTime).Hours())

	if hoursSinceRenew >= renewTimer {
		return 0, nil
	}

	return hoursSinceRenew, nil
}

var (
	// Let's Encrypt account email to use if none provided
	DefaultEmail string

	// Whether user has agreed to the Let's Encrypt SA
	Agreed bool
)

// Some essential values related to the Let's Encrypt process
const (
	// The base URL to the Let's Encrypt CA
	// TODO: Staging API URL is: https://acme-staging.api.letsencrypt.org
	caURL = "http://192.168.99.100:4000"

	// The port to expose to the CA server for Simple HTTP Challenge
	exposePort = "5001"

	// Renewal Timer - Check renewals every x hours.
	renewTimer = 24
)

// KeySize represents the length of a key in bits.
type KeySize int

// Key sizes are used to determine the strength of a key.
const (
	ECC_224  KeySize = 224
	ECC_256          = 256
	RSA_2048         = 2048
	RSA_4096         = 4096
)

// rsaKeySizeToUse is the size to use for new RSA keys.
// This shouldn't need to change except for in tests;
// the size can be drastically reduced for speed.
var rsaKeySizeToUse = RSA_2048

// CertificateMeta is a container type used to write out a file
// with information about a certificate.
type CertificateMeta struct {
	Domain, URL string
}