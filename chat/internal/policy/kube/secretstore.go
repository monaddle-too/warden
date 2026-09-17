package kube

import (
	"bytes"
	"context"
	"errors"
	"strings"

	api "warden/chat/internal/kube"
)

// The keys of the provider login Secrets (appendix A): one per file of the
// sbx shapes.
const (
	CodexAuthKey        = "auth.json"
	ClaudeAuthKey       = "claude.json"
	GitHubAuthKey       = "github.json"
	GitHubBrokerKey     = "broker.json"
	GitHubPrivateKeyKey = "app-private-key.pem"
)

// CredentialName is the store name of one key of one Secret:
// "<secret>/<key>", what the loaders' Name fields hold.
func CredentialName(secret, key string) string { return secret + "/" + key }

// SecretCredentials is the CredentialStore of the kubernetes kind
// (decision 8): a name is "<secret>/<key>" in Namespace. Load reads the
// key on every use; Store writes it back with the Secret's resourceVersion
// and retries a conflict; Watch signals every change of the key.
type SecretCredentials struct {
	Client *api.Client
	// Namespace is the core namespace holding the provider Secrets.
	Namespace string
	// Limit is the largest credential accepted, 1 MiB when zero.
	Limit int64
}

func (s *SecretCredentials) limit() int64 {
	if s.Limit > 0 {
		return s.Limit
	}
	return 1024 * 1024
}

func splitName(name string) (secret, key string, err error) {
	secret, key, ok := strings.Cut(name, "/")
	if !ok || secret == "" || key == "" || strings.Contains(key, "/") {
		return "", "", errors.New("credential name must be <secret>/<key>")
	}
	return secret, key, nil
}

// Load returns the key's bytes; a missing Secret or key is an error.
func (s *SecretCredentials) Load(ctx context.Context, name string) ([]byte, error) {
	secret, key, err := splitName(name)
	if err != nil {
		return nil, err
	}
	var obj api.Secret
	if err := s.Client.Get(ctx, api.Secrets, s.Namespace, secret, &obj); err != nil {
		if api.IsNotFound(err) {
			return nil, errors.New("Secret " + secret + " does not exist")
		}
		return nil, err
	}
	data, ok := obj.Data[key]
	if !ok {
		if text, ok := obj.StringData[key]; ok {
			data = []byte(text)
		} else {
			return nil, errors.New("Secret " + secret + " has no key " + key)
		}
	}
	if int64(len(data)) > s.limit() {
		return nil, errors.New("Secret " + secret + " key " + key + " exceeds the size limit")
	}
	return data, nil
}

// Store replaces the key in an existing Secret (never creating one: the
// operator seeds logins), conditionally on the resourceVersion read, and
// retries when another writer got there first.
func (s *SecretCredentials) Store(ctx context.Context, name string, data []byte) error {
	secret, key, err := splitName(name)
	if err != nil {
		return err
	}
	if int64(len(data)) > s.limit() {
		return errors.New("credential exceeds its size limit")
	}
	for attempt := 0; ; attempt++ {
		var obj api.Secret
		if err := s.Client.Get(ctx, api.Secrets, s.Namespace, secret, &obj); err != nil {
			if api.IsNotFound(err) {
				return errors.New("Secret " + secret + " does not exist")
			}
			return err
		}
		if obj.Data == nil {
			obj.Data = map[string][]byte{}
		}
		obj.Data[key] = append([]byte(nil), data...)
		delete(obj.StringData, key)
		err := s.Client.Update(ctx, api.Secrets, s.Namespace, secret, &obj, nil)
		if err == nil {
			return nil
		}
		if !api.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

// Watch follows the Secret by name (a metadata.name field selector, which
// the Role's resourceNames require) and signals when the key's value
// changes, appears or disappears, coalescing bursts. The channel closes
// when ctx ends.
func (s *SecretCredentials) Watch(ctx context.Context, name string) (<-chan struct{}, error) {
	secret, key, err := splitName(name)
	if err != nil {
		return nil, err
	}
	events, err := s.Client.ListWatch(ctx, api.Secrets, s.Namespace, api.ListOptions{FieldSelector: "metadata.name=" + secret})
	if err != nil {
		return nil, err
	}
	changes := make(chan struct{}, 1)
	go func() {
		defer close(changes)
		var last []byte
		present, synced := false, false
		for ev := range events {
			switch ev.Type {
			case api.Synced:
				synced = true
				continue
			case api.Added, api.Modified:
				var obj api.Secret
				if ev.Decode(&obj) != nil {
					continue
				}
				value, ok := obj.Data[key]
				if !ok {
					if text, has := obj.StringData[key]; has {
						value, ok = []byte(text), true
					}
				}
				if synced && (ok != present || !bytes.Equal(value, last)) {
					signal(changes)
				}
				present, last = ok, append([]byte(nil), value...)
			case api.Deleted:
				if synced && present {
					signal(changes)
				}
				present, last = false, nil
			}
		}
	}()
	return changes, nil
}

func signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}
