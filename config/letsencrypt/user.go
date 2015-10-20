package letsencrypt

import (
	"bufio"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"strings"

	"github.com/mholt/caddy/server"
	"github.com/xenolf/lego/acme"
)

// User represents a Let's Encrypt user account.
type User struct {
	Email        string
	Registration *acme.RegistrationResource
	KeyFile      string
	key          *rsa.PrivateKey
}

// GetEmail gets u's email.
func (u User) GetEmail() string {
	return u.Email
}

// GetRegistration gets u's registration resource.
func (u User) GetRegistration() *acme.RegistrationResource {
	return u.Registration
}

// GetPrivateKey gets u's private key.
func (u User) GetPrivateKey() *rsa.PrivateKey {
	return u.key
}

// getUser loads the user with the given email from disk.
// If the user does not exist, it will create a new one,
// but it does NOT save new users to the disk or register
// them via ACME.
func getUser(email string) (User, error) {
	var user User

	// open user file
	regFile, err := os.Open(storage.UserRegFile(email))
	if err != nil {
		if os.IsNotExist(err) {
			// create a new user
			return newUser(email)
		}
		return user, err
	}
	defer regFile.Close()

	// load user information
	err = json.NewDecoder(regFile).Decode(&user)
	if err != nil {
		return user, err
	}

	// load their private key
	user.key, err = loadRSAPrivateKey(user.KeyFile)
	if err != nil {
		return user, err
	}

	return user, nil
}

// saveUser persists a user's key and account registration
// to the file system. It does NOT register the user via ACME.
func saveUser(user User) error {
	// make user account folder
	err := os.MkdirAll(storage.User(user.Email), 0700)
	if err != nil {
		return err
	}

	// save private key file
	user.KeyFile = storage.UserKeyFile(user.Email)
	err = saveRSAPrivateKey(user.key, user.KeyFile)
	if err != nil {
		return err
	}

	// save registration file
	jsonBytes, err := json.MarshalIndent(&user, "", "\t")
	if err != nil {
		return err
	}

	return ioutil.WriteFile(storage.UserRegFile(user.Email), jsonBytes, 0600)
}

// newUser creates a new User for the given email address
// with a new private key. This function does NOT save the
// user to disk or register it via ACME. If you want to use
// a user account that might already exist, call getUser
// instead.
func newUser(email string) (User, error) {
	user := User{Email: email}
	privateKey, err := rsa.GenerateKey(rand.Reader, rsaKeySizeToUse)
	if err != nil {
		return user, errors.New("error generating private key: " + err.Error())
	}
	user.key = privateKey
	return user, nil
}

// getEmail does everything it can to obtain an email
// address from the user to use for TLS for cfg. If it
// cannot get an email address, it returns empty string.
func getEmail(cfg server.Config) string {
	// First try the tls directive from the Caddyfile
	leEmail := cfg.TLS.LetsEncryptEmail
	if leEmail == "" {
		// Then try memory (command line flag or typed by user previously)
		leEmail = DefaultEmail
	}
	if leEmail == "" {
		// Then try to get most recent user email ~/.caddy/users file
		// TODO: Probably better to open the user's json file and read the email out of there...
		userDirs, err := ioutil.ReadDir(storage.Users())
		if err == nil {
			var mostRecent os.FileInfo
			for _, dir := range userDirs {
				if !dir.IsDir() {
					continue
				}
				if mostRecent == nil || dir.ModTime().After(mostRecent.ModTime()) {
					mostRecent = dir
				}
			}
			if mostRecent != nil {
				leEmail = mostRecent.Name()
			}
		}
	}
	if leEmail == "" {
		// Alas, we must bother the user and ask for an email address
		// TODO/BUG: This doesn't work when Caddyfile is piped into caddy
		reader := bufio.NewReader(stdin)
		fmt.Print("Email address: ") // TODO: More explanation probably, and show ToS?
		var err error
		leEmail, err = reader.ReadString('\n')
		if err != nil {
			return ""
		}
		DefaultEmail = leEmail
	}
	return strings.TrimSpace(leEmail)
}

// stdin is used to read the user's input if prompted;
// this is changed by tests during tests.
var stdin = io.ReadWriter(os.Stdin)