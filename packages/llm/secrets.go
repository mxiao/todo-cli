package llm

import (
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
)

// ErrSecretNotFound means no key is stored for the account.
var ErrSecretNotFound = errors.New("secret_not_found")

// ErrSecretsUnsupported means there is no system keychain on this platform.
var ErrSecretsUnsupported = errors.New("keychain_unsupported")

// SecretStore keeps API keys outside the task database (NFR-029).
type SecretStore interface {
	Get(account string) (string, error)
	Set(account, secret string) error
	Delete(account string) error
}

// Keychain stores keys as generic passwords in the macOS login keychain
// through /usr/bin/security, so access follows the system's own prompts.
type Keychain struct {
	Service string
	// run executes security; tests replace it.
	run func(args ...string) (string, error)
}

// NewKeychain returns the system keychain store (macOS only).
func NewKeychain() SecretStore {
	return &Keychain{Service: KeychainService}
}

func (k *Keychain) exec(args ...string) (string, error) {
	if k.run != nil {
		return k.run(args...)
	}
	if runtime.GOOS != "darwin" {
		return "", fmt.Errorf("%w: the macOS keychain is only available on macOS; use an environment variable", ErrSecretsUnsupported)
	}
	out, err := exec.Command("/usr/bin/security", args...).Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return "", ErrSecretNotFound
		}
		msg := ""
		if ee != nil {
			msg = strings.TrimSpace(string(ee.Stderr))
		}
		return "", fmt.Errorf("security %s: %v %s", args[0], err, msg)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

func (k *Keychain) Get(account string) (string, error) {
	return k.exec("find-generic-password", "-s", k.Service, "-a", account, "-w")
}

func (k *Keychain) Set(account, secret string) error {
	_, err := k.exec("add-generic-password", "-U", "-s", k.Service, "-a", account, "-l", "todo-cli model API key", "-w", secret)
	return err
}

func (k *Keychain) Delete(account string) error {
	_, err := k.exec("delete-generic-password", "-s", k.Service, "-a", account)
	return err
}

// MemorySecrets is an in-memory SecretStore for tests.
type MemorySecrets struct {
	mu sync.Mutex
	m  map[string]string
}

func (m *MemorySecrets) Get(account string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	v, ok := m.m[account]
	if !ok {
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (m *MemorySecrets) Set(account, secret string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.m == nil {
		m.m = map[string]string{}
	}
	m.m[account] = secret
	return nil
}

func (m *MemorySecrets) Delete(account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.m[account]; !ok {
		return ErrSecretNotFound
	}
	delete(m.m, account)
	return nil
}
