package main

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	configFileVersion = 1
	vaultFileVersion  = 1
)

type appConfig struct {
	Version         int                 `json:"version"`
	DefaultRemoteID string              `json:"default_remote_id,omitempty"`
	Registries      []registryProfile   `json:"registries"`
	Credentials     []credentialProfile `json:"credentials"`
	Remotes         []remoteProfile     `json:"remotes"`
	ImageLists      []imageListProfile  `json:"image_lists"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

type registryProfile struct {
	ID                  string    `json:"id"`
	Name                string    `json:"name"`
	Kind                string    `json:"kind"`
	Endpoint            string    `json:"endpoint"`
	Namespace           string    `json:"namespace,omitempty"`
	Proxy               string    `json:"proxy,omitempty"`
	Insecure            bool      `json:"insecure,omitempty"`
	DefaultCredentialID string    `json:"default_credential_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

type credentialProfile struct {
	ID         string    `json:"id"`
	RegistryID string    `json:"registry_id"`
	Name       string    `json:"name"`
	Username   string    `json:"username"`
	HasSecret  bool      `json:"has_secret"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type remoteProfile struct {
	ID                 string    `json:"id"`
	Name               string    `json:"name"`
	Address            string    `json:"address"`
	User               string    `json:"user"`
	Port               int       `json:"port"`
	AuthMethod         string    `json:"auth_method"`
	KeyPath            string    `json:"key_path,omitempty"`
	HostKeyFingerprint string    `json:"host_key_fingerprint,omitempty"`
	Workspace          string    `json:"workspace"`
	DiaPath            string    `json:"dia_path,omitempty"`
	StorageRoots       []string  `json:"storage_roots,omitempty"`
	HasSecret          bool      `json:"has_secret"`
	CreatedAt          time.Time `json:"created_at"`
	UpdatedAt          time.Time `json:"updated_at"`
}

type imageListProfile struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type vaultFile struct {
	Version    int    `json:"version"`
	Nonce      string `json:"nonce"`
	Ciphertext string `json:"ciphertext"`
}

type vaultPlaintext struct {
	Secrets map[string]string `json:"secrets"`
}

type configStore struct {
	mu         sync.RWMutex
	dir        string
	configPath string
	vaultPath  string
	keyPath    string
	config     appConfig
	secrets    map[string]string
	key        []byte
}

func defaultConfigDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("DIA_CONFIG_DIR")); dir != "" {
		return filepath.Abs(filepath.Clean(dir))
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user config directory: %w", err)
	}
	return filepath.Join(dir, "dia"), nil
}

func openConfigStore(dir string) (*configStore, error) {
	if strings.TrimSpace(dir) == "" {
		var err error
		dir, err = defaultConfigDir()
		if err != nil {
			return nil, err
		}
	}
	absDir, err := filepath.Abs(filepath.Clean(dir))
	if err != nil {
		return nil, fmt.Errorf("resolve config path: %w", err)
	}
	if err := os.MkdirAll(absDir, 0o700); err != nil {
		return nil, fmt.Errorf("create config directory: %w", err)
	}
	_ = os.Chmod(absDir, 0o700)

	store := &configStore{
		dir:        absDir,
		configPath: filepath.Join(absDir, "config.json"),
		vaultPath:  filepath.Join(absDir, "secrets.enc"),
		keyPath:    filepath.Join(absDir, "master.key"),
		config: appConfig{
			Version:     configFileVersion,
			Registries:  []registryProfile{},
			Credentials: []credentialProfile{},
			Remotes:     []remoteProfile{},
			ImageLists:  []imageListProfile{},
		},
		secrets: make(map[string]string),
	}
	if err := store.load(); err != nil {
		return nil, err
	}
	return store, nil
}

func (s *configStore) load() error {
	if data, err := os.ReadFile(s.configPath); err == nil {
		if err := json.Unmarshal(data, &s.config); err != nil {
			return fmt.Errorf("parse config: %w", err)
		}
		if s.config.Version != configFileVersion {
			return fmt.Errorf("unsupported config version %d", s.config.Version)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read config: %w", err)
	}

	if _, err := os.Stat(s.vaultPath); err == nil {
		key, err := s.loadMasterKey(false)
		if err != nil {
			return err
		}
		s.key = key
		if err := s.loadVault(); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat secret vault: %w", err)
	}
	if s.config.Registries == nil {
		s.config.Registries = []registryProfile{}
	}
	if s.config.Credentials == nil {
		s.config.Credentials = []credentialProfile{}
	}
	if s.config.Remotes == nil {
		s.config.Remotes = []remoteProfile{}
	}
	if s.config.ImageLists == nil {
		s.config.ImageLists = []imageListProfile{}
	}
	s.refreshSecretFlagsLocked()
	return nil
}

func (s *configStore) snapshot() appConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	copyConfig := s.config
	copyConfig.Registries = append([]registryProfile{}, s.config.Registries...)
	copyConfig.Credentials = append([]credentialProfile{}, s.config.Credentials...)
	copyConfig.Remotes = append([]remoteProfile{}, s.config.Remotes...)
	copyConfig.ImageLists = append([]imageListProfile{}, s.config.ImageLists...)
	for i := range copyConfig.Remotes {
		copyConfig.Remotes[i].StorageRoots = append([]string(nil), copyConfig.Remotes[i].StorageRoots...)
	}
	return copyConfig
}

func (s *configStore) upsertRegistry(profile registryProfile) (registryProfile, error) {
	if err := validateRegistryProfile(profile); err != nil {
		return registryProfile{}, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.Registries, func(item registryProfile) bool { return item.ID == profile.ID })
	if idx < 0 {
		if profile.ID != "" {
			return registryProfile{}, fmt.Errorf("registry profile not found")
		}
		profile.ID = newConfigID("reg")
		profile.CreatedAt = now
	} else {
		profile.CreatedAt = s.config.Registries[idx].CreatedAt
	}
	profile.UpdatedAt = now
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Kind = strings.ToLower(strings.TrimSpace(profile.Kind))
	if profile.Kind == "" {
		profile.Kind = "registry"
	}
	profile.Endpoint = normalizeRegistryEndpoint(profile.Endpoint)
	profile.Namespace = strings.Trim(strings.TrimSpace(profile.Namespace), "/")
	profile.Proxy = strings.TrimSpace(profile.Proxy)
	if idx < 0 {
		s.config.Registries = append(s.config.Registries, profile)
	} else {
		s.config.Registries[idx] = profile
	}
	if err := s.saveConfigLocked(); err != nil {
		return registryProfile{}, err
	}
	return profile, nil
}

func (s *configStore) deleteRegistry(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.Registries, func(item registryProfile) bool { return item.ID == id })
	if idx < 0 {
		return fmt.Errorf("registry profile not found")
	}
	s.config.Registries = append(s.config.Registries[:idx], s.config.Registries[idx+1:]...)
	for i := len(s.config.Credentials) - 1; i >= 0; i-- {
		if s.config.Credentials[i].RegistryID == id {
			delete(s.secrets, credentialSecretKey(s.config.Credentials[i].ID))
			s.config.Credentials = append(s.config.Credentials[:i], s.config.Credentials[i+1:]...)
		}
	}
	if err := s.saveConfigLocked(); err != nil {
		return err
	}
	return s.saveVaultLocked()
}

func (s *configStore) upsertCredential(profile credentialProfile, secret *string) (credentialProfile, error) {
	if strings.TrimSpace(profile.RegistryID) == "" || strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Username) == "" {
		return credentialProfile{}, fmt.Errorf("registry, account name, and username are required")
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.hasRegistryLocked(profile.RegistryID) {
		return credentialProfile{}, fmt.Errorf("registry profile not found")
	}
	idx := slices.IndexFunc(s.config.Credentials, func(item credentialProfile) bool { return item.ID == profile.ID })
	if idx < 0 {
		if profile.ID != "" {
			return credentialProfile{}, fmt.Errorf("credential profile not found")
		}
		profile.ID = newConfigID("cred")
		profile.CreatedAt = now
	} else {
		profile.CreatedAt = s.config.Credentials[idx].CreatedAt
	}
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Username = strings.TrimSpace(profile.Username)
	profile.UpdatedAt = now
	if secret != nil {
		secretKey := credentialSecretKey(profile.ID)
		if *secret == "" {
			delete(s.secrets, secretKey)
		} else {
			s.secrets[secretKey] = *secret
		}
	}
	profile.HasSecret = s.secrets[credentialSecretKey(profile.ID)] != ""
	if idx < 0 {
		s.config.Credentials = append(s.config.Credentials, profile)
	} else {
		s.config.Credentials[idx] = profile
	}
	if secret != nil {
		if err := s.saveVaultLocked(); err != nil {
			return credentialProfile{}, err
		}
	}
	if err := s.saveConfigLocked(); err != nil {
		return credentialProfile{}, err
	}
	return profile, nil
}

func (s *configStore) deleteCredential(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.Credentials, func(item credentialProfile) bool { return item.ID == id })
	if idx < 0 {
		return fmt.Errorf("credential profile not found")
	}
	registryID := s.config.Credentials[idx].RegistryID
	s.config.Credentials = append(s.config.Credentials[:idx], s.config.Credentials[idx+1:]...)
	delete(s.secrets, credentialSecretKey(id))
	for i := range s.config.Registries {
		if s.config.Registries[i].ID == registryID && s.config.Registries[i].DefaultCredentialID == id {
			s.config.Registries[i].DefaultCredentialID = ""
			s.config.Registries[i].UpdatedAt = time.Now().UTC()
		}
	}
	if err := s.saveConfigLocked(); err != nil {
		return err
	}
	return s.saveVaultLocked()
}

func (s *configStore) upsertRemote(profile remoteProfile, secret *string) (remoteProfile, error) {
	if profile.Port == 0 {
		profile.Port = 22
	}
	if strings.TrimSpace(profile.AuthMethod) == "" {
		profile.AuthMethod = "agent"
	}
	if err := validateRemoteProfile(profile); err != nil {
		return remoteProfile{}, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.Remotes, func(item remoteProfile) bool { return item.ID == profile.ID })
	if idx < 0 {
		if profile.ID != "" {
			return remoteProfile{}, fmt.Errorf("remote profile not found")
		}
		profile.ID = newConfigID("host")
		profile.CreatedAt = now
	} else {
		profile.CreatedAt = s.config.Remotes[idx].CreatedAt
	}
	profile.Name = strings.TrimSpace(profile.Name)
	profile.Address = strings.TrimSpace(profile.Address)
	profile.User = strings.TrimSpace(profile.User)
	profile.AuthMethod = strings.ToLower(strings.TrimSpace(profile.AuthMethod))
	profile.KeyPath = strings.TrimSpace(profile.KeyPath)
	profile.HostKeyFingerprint = strings.TrimSpace(profile.HostKeyFingerprint)
	profile.Workspace = strings.TrimSpace(profile.Workspace)
	profile.DiaPath = strings.TrimSpace(profile.DiaPath)
	profile.StorageRoots = cleanUniqueStrings(profile.StorageRoots)
	profile.UpdatedAt = now
	if secret != nil {
		secretKey := remoteSecretKey(profile.ID)
		if *secret == "" {
			delete(s.secrets, secretKey)
		} else {
			s.secrets[secretKey] = *secret
		}
	}
	profile.HasSecret = s.secrets[remoteSecretKey(profile.ID)] != ""
	if idx < 0 {
		s.config.Remotes = append(s.config.Remotes, profile)
	} else {
		s.config.Remotes[idx] = profile
	}
	if secret != nil {
		if err := s.saveVaultLocked(); err != nil {
			return remoteProfile{}, err
		}
	}
	if err := s.saveConfigLocked(); err != nil {
		return remoteProfile{}, err
	}
	return profile, nil
}

func (s *configStore) deleteRemote(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.Remotes, func(item remoteProfile) bool { return item.ID == id })
	if idx < 0 {
		return fmt.Errorf("remote profile not found")
	}
	s.config.Remotes = append(s.config.Remotes[:idx], s.config.Remotes[idx+1:]...)
	delete(s.secrets, remoteSecretKey(id))
	if s.config.DefaultRemoteID == id {
		s.config.DefaultRemoteID = ""
	}
	if err := s.saveConfigLocked(); err != nil {
		return err
	}
	return s.saveVaultLocked()
}

func (s *configStore) setDefaultRemote(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id != "" && slices.IndexFunc(s.config.Remotes, func(item remoteProfile) bool { return item.ID == id }) < 0 {
		return fmt.Errorf("remote profile not found")
	}
	s.config.DefaultRemoteID = id
	return s.saveConfigLocked()
}

func (s *configStore) upsertImageList(list imageListProfile) (imageListProfile, error) {
	if strings.TrimSpace(list.Name) == "" {
		return imageListProfile{}, fmt.Errorf("list name is required")
	}
	if _, err := parseImageList(list.Content); err != nil {
		return imageListProfile{}, err
	}
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.ImageLists, func(item imageListProfile) bool { return item.ID == list.ID })
	if idx < 0 {
		if list.ID != "" {
			return imageListProfile{}, fmt.Errorf("image list not found")
		}
		list.ID = newConfigID("list")
		list.CreatedAt = now
	} else {
		list.CreatedAt = s.config.ImageLists[idx].CreatedAt
	}
	list.Name = strings.TrimSpace(list.Name)
	list.Content = normalizeListContent(list.Content)
	list.UpdatedAt = now
	if idx < 0 {
		s.config.ImageLists = append(s.config.ImageLists, list)
	} else {
		s.config.ImageLists[idx] = list
	}
	if err := s.saveConfigLocked(); err != nil {
		return imageListProfile{}, err
	}
	return list, nil
}

func (s *configStore) deleteImageList(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := slices.IndexFunc(s.config.ImageLists, func(item imageListProfile) bool { return item.ID == id })
	if idx < 0 {
		return fmt.Errorf("image list not found")
	}
	s.config.ImageLists = append(s.config.ImageLists[:idx], s.config.ImageLists[idx+1:]...)
	return s.saveConfigLocked()
}

func (s *configStore) registry(id string) (registryProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := slices.IndexFunc(s.config.Registries, func(item registryProfile) bool { return item.ID == id })
	if idx < 0 {
		return registryProfile{}, fmt.Errorf("registry profile not found")
	}
	return s.config.Registries[idx], nil
}

func (s *configStore) credential(id string) (credentialProfile, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := slices.IndexFunc(s.config.Credentials, func(item credentialProfile) bool { return item.ID == id })
	if idx < 0 {
		return credentialProfile{}, "", fmt.Errorf("credential profile not found")
	}
	profile := s.config.Credentials[idx]
	return profile, s.secrets[credentialSecretKey(id)], nil
}

func (s *configStore) remote(id string) (remoteProfile, string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := slices.IndexFunc(s.config.Remotes, func(item remoteProfile) bool { return item.ID == id })
	if idx < 0 {
		return remoteProfile{}, "", fmt.Errorf("remote profile not found")
	}
	profile := s.config.Remotes[idx]
	profile.StorageRoots = append([]string(nil), profile.StorageRoots...)
	return profile, s.secrets[remoteSecretKey(id)], nil
}

func (s *configStore) imageList(id string) (imageListProfile, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	idx := slices.IndexFunc(s.config.ImageLists, func(item imageListProfile) bool { return item.ID == id })
	if idx < 0 {
		return imageListProfile{}, fmt.Errorf("image list not found")
	}
	return s.config.ImageLists[idx], nil
}

func (s *configStore) saveConfigLocked() error {
	s.config.Version = configFileVersion
	s.config.UpdatedAt = time.Now().UTC()
	s.refreshSecretFlagsLocked()
	data, err := json.MarshalIndent(s.config, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	data = append(data, '\n')
	return writeFileAtomic(s.configPath, data, 0o600)
}

func (s *configStore) loadMasterKey(create bool) ([]byte, error) {
	if encoded := strings.TrimSpace(os.Getenv("DIA_CONFIG_KEY")); encoded != "" {
		key, err := decodeConfigKey(encoded)
		if err != nil {
			return nil, fmt.Errorf("decode DIA_CONFIG_KEY: %w", err)
		}
		return key, nil
	}
	data, err := os.ReadFile(s.keyPath)
	if err == nil {
		key, err := base64.RawStdEncoding.DecodeString(strings.TrimSpace(string(data)))
		if err != nil || len(key) != 32 {
			return nil, fmt.Errorf("invalid master key file")
		}
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("read master key: %w", err)
	}
	if !create {
		return nil, fmt.Errorf("secret vault exists but master key is missing")
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, fmt.Errorf("generate master key: %w", err)
	}
	if err := writeFileAtomic(s.keyPath, []byte(base64.RawStdEncoding.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func decodeConfigKey(encoded string) ([]byte, error) {
	for _, encoding := range []*base64.Encoding{base64.RawStdEncoding, base64.StdEncoding, base64.RawURLEncoding, base64.URLEncoding} {
		if key, err := encoding.DecodeString(encoded); err == nil && len(key) == 32 {
			return key, nil
		}
	}
	return nil, fmt.Errorf("key must be 32 bytes encoded as base64")
}

func (s *configStore) loadVault() error {
	data, err := os.ReadFile(s.vaultPath)
	if err != nil {
		return fmt.Errorf("read secret vault: %w", err)
	}
	var file vaultFile
	if err := json.Unmarshal(data, &file); err != nil {
		return fmt.Errorf("parse secret vault: %w", err)
	}
	if file.Version != vaultFileVersion {
		return fmt.Errorf("unsupported secret vault version %d", file.Version)
	}
	nonce, err := base64.RawStdEncoding.DecodeString(file.Nonce)
	if err != nil {
		return fmt.Errorf("decode secret vault nonce: %w", err)
	}
	ciphertext, err := base64.RawStdEncoding.DecodeString(file.Ciphertext)
	if err != nil {
		return fmt.Errorf("decode secret vault payload: %w", err)
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return fmt.Errorf("initialize secret vault cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("initialize secret vault AEAD: %w", err)
	}
	plaintext, err := gcm.Open(nil, nonce, ciphertext, []byte("dia-secrets-v1"))
	if err != nil {
		return fmt.Errorf("decrypt secret vault: %w", err)
	}
	var decoded vaultPlaintext
	if err := json.Unmarshal(plaintext, &decoded); err != nil {
		return fmt.Errorf("parse decrypted secret vault: %w", err)
	}
	if decoded.Secrets == nil {
		decoded.Secrets = make(map[string]string)
	}
	s.secrets = decoded.Secrets
	return nil
}

func (s *configStore) saveVaultLocked() error {
	if len(s.secrets) == 0 {
		if err := os.Remove(s.vaultPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove empty secret vault: %w", err)
		}
		return nil
	}
	if len(s.key) == 0 {
		key, err := s.loadMasterKey(true)
		if err != nil {
			return err
		}
		s.key = key
	}
	plaintext, err := json.Marshal(vaultPlaintext{Secrets: s.secrets})
	if err != nil {
		return fmt.Errorf("encode secret vault: %w", err)
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return fmt.Errorf("initialize secret vault cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("initialize secret vault AEAD: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return fmt.Errorf("generate secret vault nonce: %w", err)
	}
	ciphertext := gcm.Seal(nil, nonce, plaintext, []byte("dia-secrets-v1"))
	data, err := json.MarshalIndent(vaultFile{
		Version:    vaultFileVersion,
		Nonce:      base64.RawStdEncoding.EncodeToString(nonce),
		Ciphertext: base64.RawStdEncoding.EncodeToString(ciphertext),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode secret vault envelope: %w", err)
	}
	return writeFileAtomic(s.vaultPath, append(data, '\n'), 0o600)
}

func (s *configStore) refreshSecretFlagsLocked() {
	for i := range s.config.Credentials {
		s.config.Credentials[i].HasSecret = s.secrets[credentialSecretKey(s.config.Credentials[i].ID)] != ""
	}
	for i := range s.config.Remotes {
		s.config.Remotes[i].HasSecret = s.secrets[remoteSecretKey(s.config.Remotes[i].ID)] != ""
	}
}

func (s *configStore) hasRegistryLocked(id string) bool {
	return slices.IndexFunc(s.config.Registries, func(item registryProfile) bool { return item.ID == id }) >= 0
}

func validateRegistryProfile(profile registryProfile) error {
	if strings.TrimSpace(profile.Name) == "" {
		return fmt.Errorf("registry name is required")
	}
	kind := strings.ToLower(strings.TrimSpace(profile.Kind))
	if kind == "" {
		kind = "registry"
	}
	if kind != "registry" && kind != "harbor" {
		return fmt.Errorf("registry kind must be registry or harbor")
	}
	endpoint := normalizeRegistryEndpoint(profile.Endpoint)
	u, err := url.Parse(endpoint)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("registry endpoint must be an http:// or https:// URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("registry endpoint must not contain credentials, query, or fragment")
	}
	if escapedPath := strings.TrimRight(u.EscapedPath(), "/"); escapedPath != "" {
		return fmt.Errorf("registry endpoint must be an origin without a path")
	}
	namespace := strings.Trim(strings.TrimSpace(profile.Namespace), "/")
	if namespace != "" {
		if err := validateRepositoryPath(namespace); err != nil {
			return fmt.Errorf("invalid registry namespace: %w", err)
		}
	}
	if proxy := strings.TrimSpace(profile.Proxy); proxy != "" {
		parsedProxy, err := parseExplicitProxyURL(proxy)
		if err != nil {
			return err
		}
		if parsedProxy.User != nil {
			return fmt.Errorf("authenticated proxy URLs cannot be saved in a Registry profile; use an execution-machine proxy environment or a temporary proxy")
		}
	}
	return nil
}

func normalizeRegistryEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint != "" && !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	return strings.TrimRight(endpoint, "/")
}

func validateRemoteProfile(profile remoteProfile) error {
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.Address) == "" || strings.TrimSpace(profile.User) == "" {
		return fmt.Errorf("remote name, address, and user are required")
	}
	if net.ParseIP(profile.Address) == nil && strings.ContainsAny(profile.Address, "/@ \\[]") {
		return fmt.Errorf("remote address must be a hostname or IP without a port")
	}
	if profile.Port == 0 {
		profile.Port = 22
	}
	if profile.Port < 1 || profile.Port > 65535 {
		return fmt.Errorf("remote SSH port is invalid")
	}
	authMethod := strings.ToLower(strings.TrimSpace(profile.AuthMethod))
	if authMethod == "" {
		authMethod = "agent"
	}
	if authMethod != "agent" && authMethod != "key" && authMethod != "password" {
		return fmt.Errorf("remote auth method must be agent, key, or password")
	}
	if authMethod == "key" && strings.TrimSpace(profile.KeyPath) == "" {
		return fmt.Errorf("private key path is required for key authentication")
	}
	if strings.TrimSpace(profile.Workspace) == "" {
		return fmt.Errorf("remote workspace is required")
	}
	return nil
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create parent directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".dia-write-*")
	if err != nil {
		return fmt.Errorf("create temporary file: %w", err)
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := tmp.Chmod(mode); err != nil {
		return fmt.Errorf("set temporary file permissions: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("write temporary file: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("sync temporary file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary file: %w", err)
	}
	if err := replaceFile(tmpPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	ok = true
	return nil
}

func credentialSecretKey(id string) string { return "credential:" + id }
func remoteSecretKey(id string) string     { return "remote:" + id }

func newConfigID(prefix string) string {
	random := make([]byte, 10)
	if _, err := io.ReadFull(rand.Reader, random); err == nil {
		return prefix + "_" + base64.RawURLEncoding.EncodeToString(random)
	}
	return prefix + "_" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

func cleanUniqueStrings(values []string) []string {
	result := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

type imageListItem struct {
	Source string `json:"source"`
	Target string `json:"target"`
	Line   int    `json:"line"`
}

func parseImageList(content string) ([]imageListItem, error) {
	items := make([]imageListItem, 0)
	for index, rawLine := range strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		source := line
		target := line
		if before, after, ok := strings.Cut(line, "->"); ok {
			source = strings.TrimSpace(before)
			target = strings.TrimSpace(after)
		}
		if source == "" || target == "" {
			return nil, fmt.Errorf("image list line %d has an empty source or target", index+1)
		}
		if _, err := parseImageRef(source); err != nil {
			return nil, fmt.Errorf("image list line %d source: %w", index+1, err)
		}
		if _, err := parseImageRef(target); err != nil {
			return nil, fmt.Errorf("image list line %d target: %w", index+1, err)
		}
		items = append(items, imageListItem{Source: source, Target: target, Line: index + 1})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("image list contains no images")
	}
	return items, nil
}

func normalizeListContent(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	return strings.TrimRight(content, "\n") + "\n"
}
