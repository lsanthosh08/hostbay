// Package backup writes Hostbay backups, reads them and SSH Manager's
// .sshmbak files, and merges a backup into the store (KTD12).
package backup

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/crypto/argon2"

	"hostbay/internal/apperr"
	"hostbay/internal/secrets"
	"hostbay/internal/store"
)

// Bundle is the content of a backup, in SSH Manager's StoreBundle shape plus
// each item's Meta and Hostbay's proxies. Inside an encrypted backup
// Host.Password and Proxy.Password are plaintext and Keys maps key-store
// names to base64 key files; a plain backup has none of them. A Bundle
// never reaches the store as is: Restore re-seals.
type Bundle struct {
	SchemaVersion int               `json:"schemaVersion,omitempty"`
	Hosts         []store.Host      `json:"hosts"`
	Folders       []store.Folder    `json:"folders"`
	Scripts       []store.Script    `json:"scripts"`
	Bookmarks     []string          `json:"bookmarks"`
	Keys          map[string]string `json:"keys,omitempty"`
	Proxies       []store.Proxy     `json:"proxies,omitempty"`
	// Settings are the synced groups; local.json is never in a backup.
	Settings []store.SettingsGroup `json:"settings,omitempty"`
	// PostLogin are the post-login rules. Answer is plaintext inside an
	// encrypted backup and absent from a plain one.
	PostLogin []store.PostLoginRule `json:"postLogin,omitempty"`
	// Workspaces hold host IDs and layouts only (KTD40).
	Workspaces []store.Workspace `json:"workspaces,omitempty"`
	// AIProviders' APIKey is plaintext in an encrypted backup and absent
	// from a plain one, like a host's password.
	AIProviders []store.AIProvider `json:"aiProviders,omitempty"`
	// Skills are skills and prompt templates; they hold no secret.
	Skills []store.Skill `json:"skills,omitempty"`
}

// bundleVersion is the newest bundle this build reads. A bundle holding
// settings, post-login rules or workspaces is written as 4, so a build that would drop them refuses it;
// one holding a database or Kubernetes host as 3, so a build that would
// drop its fields refuses it; one whose hosts use proxies as 2, so a build
// that would drop the proxy and connect directly refuses it; any other
// stays 1.
const bundleVersion = 4 // AI providers and skills are written as 4 too

// Hostbay's encrypted layout:
//
//	"HBBACKUP" | version(1)=1 | argon2 time(4) | memory KiB(4) | threads(1) |
//	salt(16) | nonce(12) | AES-256-GCM ciphertext+tag
//
// The key is Argon2id(passphrase, salt) with the stored parameters, and the
// whole header is the GCM additional data, so a changed parameter fails to
// open. Writes use RFC 9106's second recommended setting (t=3, 64 MiB, p=4),
// stronger than SSH Manager's t=2, 19 MiB, p=1 (R16).
const (
	magic       = "HBBACKUP"
	saltLen     = 16
	headerLen   = len(magic) + 1 + 4 + 4 + 1 + saltLen + nonceLen
	writeTime   = 3
	writeMemory = 64 * 1024
	writeThread = 4
	maxMemory   = 1024 * 1024 // refuse files asking for more than 1 GiB
	maxTime     = 16
)

// Encrypt seals plain under passphrase in Hostbay's format.
func Encrypt(plain []byte, passphrase string) ([]byte, error) {
	if passphrase == "" {
		return nil, errors.New("passphrase required")
	}
	h := make([]byte, headerLen)
	copy(h, magic)
	h[8] = 1
	binary.BigEndian.PutUint32(h[9:], writeTime)
	binary.BigEndian.PutUint32(h[13:], writeMemory)
	h[17] = writeThread
	if _, err := rand.Read(h[18:]); err != nil { // salt and nonce
		return nil, err
	}
	gcm, err := newGCM(argon2.IDKey([]byte(passphrase), h[18:18+saltLen], writeTime, writeMemory, writeThread, 32))
	if err != nil {
		return nil, err
	}
	return gcm.Seal(h, h[18+saltLen:], plain, h), nil
}

// Decrypt opens a Hostbay encrypted backup. A wrong passphrase or any
// tampering returns an error and no output.
func Decrypt(data []byte, passphrase string) ([]byte, error) {
	if len(data) < headerLen || string(data[:len(magic)]) != magic {
		return nil, errors.New("not a Hostbay backup")
	}
	if data[8] != 1 {
		return nil, fmt.Errorf("backup format version %d is newer than this Hostbay; update Hostbay", data[8])
	}
	t, m, p := binary.BigEndian.Uint32(data[9:]), binary.BigEndian.Uint32(data[13:]), data[17]
	if t == 0 || t > maxTime || m < 8*uint32(p) || m > maxMemory || p == 0 {
		return nil, errors.New("backup has invalid key parameters")
	}
	h := data[:headerLen]
	return openGCM(argon2.IDKey([]byte(passphrase), h[18:18+saltLen], t, m, p, 32), h[18+saltLen:], data[headerLen:], h)
}

// Read decodes a backup file of any kind, told apart by its first bytes:
// Hostbay encrypted, SSH Manager .sshmbak, or plain JSON from either app.
func Read(data []byte, passphrase string) (Bundle, error) {
	var b Bundle
	encrypted := bytes.HasPrefix(data, []byte(magic)) || bytes.HasPrefix(data, []byte(sshmMagic))
	if encrypted && passphrase == "" {
		return b, errors.New("this backup is encrypted — passphrase required")
	}
	var err error
	switch {
	case bytes.HasPrefix(data, []byte(magic)):
		data, err = Decrypt(data, passphrase)
	case bytes.HasPrefix(data, []byte(sshmMagic)):
		data, err = decryptSSHM(data, passphrase)
	}
	if err != nil {
		return b, err
	}
	if err := json.Unmarshal(data, &b); err != nil {
		return Bundle{}, fmt.Errorf("not a backup file: %w", err)
	}
	if b.SchemaVersion > bundleVersion {
		return Bundle{}, fmt.Errorf("backup schema version %d is newer than this Hostbay; update Hostbay", b.SchemaVersion)
	}
	return b, nil
}

// Export writes the whole store, or only host hostID, as a backup. With a
// passphrase it is encrypted and carries passwords and key-store keys; with
// none it is plain JSON with every secret removed (R16). Callers must pass
// OS verification first. A password that no longer opens (its host was
// pointed elsewhere) is left out; any other failure to unseal stops the
// export.
func Export(d *store.Data, v *secrets.Vault, hostID, passphrase string) ([]byte, error) {
	withSecrets := passphrase != ""
	b := Bundle{Hosts: []store.Host{}, Folders: []store.Folder{}, Scripts: []store.Script{}, Bookmarks: []string{}}
	if hostID == "" {
		b.Hosts = append(b.Hosts, d.Hosts.Items...)
		b.Folders = append(b.Folders, d.Folders.Items...)
		b.Scripts = append(b.Scripts, d.Scripts.Items...)
		b.Bookmarks = d.BookmarkPaths()
		b.Proxies = append(b.Proxies, d.Proxies.Items...)
		b.Settings = d.Settings.Items
		b.PostLogin = append(b.PostLogin, d.PostLogin.Items...)
		b.Workspaces = d.Workspaces.Items
		for _, p := range d.AIProviders.Items {
			if !p.LocalOnly() { // a Claude Code provider's program path means nothing on another computer
				b.AIProviders = append(b.AIProviders, p)
			}
		}
		b.Skills = d.Skills.Items
	} else if h := d.Host(hostID); h != nil {
		b.Hosts = append(b.Hosts, *h)
		b.PostLogin = d.PostLoginRules(hostID)
		if h.ProxyID != nil && d.Proxy(*h.ProxyID) != nil {
			b.Proxies = append(b.Proxies, *d.Proxy(*h.ProxyID))
		}
	} else {
		return nil, errors.New("host not found")
	}
	for i := range b.Proxies {
		p := &b.Proxies[i]
		if !withSecrets || p.Password == "" {
			p.Password = ""
			continue
		}
		pw, err := v.Open(p.Password, p.SecretAAD())
		if err != nil && !undecryptable(err) {
			return nil, fmt.Errorf("proxy %s: %w", p.Name, err)
		}
		p.Password = string(pw)
	}
	for i := range b.AIProviders {
		p := &b.AIProviders[i]
		if !withSecrets || p.APIKey == "" {
			p.APIKey = ""
			continue
		}
		k, err := v.Open(p.APIKey, p.SecretAAD())
		if err != nil && !undecryptable(err) {
			return nil, fmt.Errorf("AI provider %s: %w", p.Name, err)
		}
		p.APIKey = string(k)
	}
	for i := range b.Hosts {
		h := &b.Hosts[i]
		name, isKey := strings.CutPrefix(h.IdentityFile, store.KeyRefPrefix)
		if !withSecrets {
			h.Password = ""
			if isKey {
				h.IdentityFile = ""
			}
			continue
		}
		if h.Password != "" {
			pw, err := v.Open(h.Password, h.SecretAAD())
			if err != nil && !undecryptable(err) {
				return nil, fmt.Errorf("%s: %w", h.Alias, err)
			}
			h.Password = string(pw)
		}
		if k := d.KeyByName(name); isKey && k != nil {
			raw, err := v.Open(k.Data, store.KeyAAD(k.ID))
			if err != nil {
				return nil, fmt.Errorf("key %s: %w", name, err)
			}
			if b.Keys == nil {
				b.Keys = map[string]string{}
			}
			b.Keys[name] = base64.StdEncoding.EncodeToString(raw)
		}
	}
	for i := range b.PostLogin {
		r := &b.PostLogin[i]
		r.Held = false
		h := d.Host(r.HostID)
		if !withSecrets || r.Answer == "" || h == nil {
			r.Answer = ""
			continue
		}
		answer, err := v.Open(r.Answer, store.PostLoginAAD(r.ID, h))
		if err != nil && !undecryptable(err) {
			return nil, fmt.Errorf("post-login rule for %s: %w", h.Alias, err)
		}
		r.Answer = string(answer)
	}
	b.SchemaVersion = 1
	for _, h := range b.Hosts {
		switch {
		case h.IsDatabase() || h.KindName() == "kubernetes":
			b.SchemaVersion = 3
		case h.Proxied():
			b.SchemaVersion = max(b.SchemaVersion, 2)
		}
	}
	if len(b.Settings) > 0 || len(b.PostLogin) > 0 || len(b.Workspaces) > 0 || len(b.AIProviders) > 0 || len(b.Skills) > 0 {
		b.SchemaVersion = 4
	}
	if !withSecrets {
		return json.MarshalIndent(b, "", "  ")
	}
	plain, err := json.Marshal(b)
	if err != nil {
		return nil, err
	}
	return Encrypt(plain, passphrase)
}

// undecryptable reports a sealed value that is damaged or bound to another
// address, as opposed to a master key that cannot be had at all.
func undecryptable(err error) bool {
	var e *apperr.Error
	return errors.As(err, &e) && e.Code == apperr.SecretNeeded && e.Details["reason"] == "decrypt"
}

// Restore merges b into d and returns how many hosts it added, with a note
// for each added host that names no saved proxy. It only adds:
// hosts match by ID, then by alias, hostname, port and user together;
// folders by ID, then by name and parent; scripts by ID, then by label and
// command; keys by name and content. A matched local item is left exactly
// as it is, so restoring twice adds nothing and a later local edit stands.
// Added items keep the Meta they were backed up with. Passwords, plaintext
// or in SSH Manager's obf: form, are sealed under v.
func Restore(d *store.Data, v *secrets.Vault, b Bundle) (int, []string, error) {
	keyNames := map[string]string{} // backup name -> local name
	for name, enc := range b.Keys {
		raw, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			continue
		}
		if k := d.KeyByName(name); k != nil {
			if cur, err := v.Open(k.Data, store.KeyAAD(k.ID)); err == nil && bytes.Equal(cur, raw) {
				keyNames[name] = name
				continue
			}
		}
		id := store.NewID()
		sealed, err := v.Seal(raw, store.KeyAAD(id))
		if err != nil {
			return 0, nil, err
		}
		k, err := d.AddKey(store.Key{Meta: store.Meta{ID: id}, Name: name, Data: sealed})
		if err != nil {
			return 0, nil, err
		}
		keyNames[name] = k.Name
	}

	folderIDs := map[string]string{} // backup ID -> local ID
	backupFolders := map[string]store.Folder{}
	for _, f := range b.Folders {
		backupFolders[f.ID] = f
	}
	var resolve func(id string, depth int) (string, bool)
	resolve = func(id string, depth int) (string, bool) {
		if local, ok := folderIDs[id]; ok {
			return local, true
		}
		f, ok := backupFolders[id]
		if !ok || depth > len(b.Folders) { // unknown, or a parent cycle
			return "", false
		}
		if d.Folder(f.ID) != nil {
			folderIDs[id] = f.ID
			return f.ID, true
		}
		var parent *string
		if f.ParentID != nil {
			if p, ok := resolve(*f.ParentID, depth+1); ok {
				parent = &p
			}
		}
		for _, l := range d.Folders.Items {
			if l.Name == f.Name && equalPtr(l.ParentID, parent) {
				folderIDs[id] = l.ID
				return l.ID, true
			}
		}
		f.ParentID = parent
		folderIDs[id] = d.AddFolder(f).ID
		return folderIDs[id], true
	}
	for _, f := range b.Folders {
		resolve(f.ID, 0)
	}

	proxyIDs := map[string]string{} // backup ID -> local ID
	for _, p := range b.Proxies {
		if local := matchProxy(d, p); local != "" {
			proxyIDs[p.ID] = local
			continue
		}
		if p.ID == "" {
			p.ID = store.NewID()
		}
		pw := p.Password
		p.Password = ""
		if pw != "" {
			sealed, err := v.Seal([]byte(pw), p.SecretAAD())
			if err != nil {
				return 0, nil, err
			}
			p.Password = sealed
		}
		proxyIDs[p.ID] = d.AddProxy(p).ID
	}

	hostIDs := map[string]string{} // backup ID -> local ID
	var added []string
	for _, h := range b.Hosts {
		if local := matchHost(d, h); local != "" {
			hostIDs[h.ID] = local
			continue
		}
		if h.FolderID != nil {
			if local, ok := folderIDs[*h.FolderID]; ok {
				h.FolderID = &local
			} else {
				h.FolderID = nil
			}
		}
		if name, ok := strings.CutPrefix(h.IdentityFile, store.KeyRefPrefix); ok && keyNames[name] != "" {
			h.IdentityFile = store.KeyRefPrefix + keyNames[name]
		}
		// A proxy missing from the backup stays named, so the host fails to
		// connect rather than connect around it (KTD22).
		if h.ProxyID != nil && proxyIDs[*h.ProxyID] != "" {
			h.ProxyID = new(proxyIDs[*h.ProxyID])
		}
		if h.ID == "" {
			h.ID = store.NewID()
		}
		pw := Deobfuscate(h.Password)
		h.Password = ""
		if pw != "" {
			sealed, err := v.Seal([]byte(pw), h.SecretAAD())
			if err != nil {
				return 0, nil, err
			}
			h.Password = sealed
		}
		hostIDs[h.ID] = h.ID
		added = append(added, d.AddHost(h).ID)
	}
	// Jump links point at backup IDs; follow them to the local host. One whose
	// host is neither in the backup nor saved here becomes the missing-proxy
	// marker, so the host fails to connect rather than connect directly (KTD24).
	for _, id := range added {
		h := d.Host(id)
		if h.JumpHostID == nil {
			continue
		}
		if local, ok := hostIDs[*h.JumpHostID]; ok {
			h.JumpHostID = &local
		} else if d.Host(*h.JumpHostID) == nil {
			h.JumpHostID, h.ProxyID = nil, store.MissingProxy()
		}
	}
	// A host left naming no saved proxy cannot connect; say which.
	var notes []string
	for _, id := range added {
		if h := d.Host(id); h.Proxied() && d.Proxy(*h.ProxyID) == nil {
			notes = append(notes, h.Alias+": its jump host or proxy is not in this backup, so it will not connect until you choose its jump host or a proxy in its settings")
		}
	}

	// A rule follows its host to the local one and is sealed for it; one
	// whose host is neither in the backup nor saved here is left out.
	for _, r := range b.PostLogin {
		if r.ID == "" || d.PostLoginRule(r.ID) != nil {
			continue
		}
		if local, ok := hostIDs[r.HostID]; ok {
			r.HostID = local
		}
		h := d.Host(r.HostID)
		if h == nil {
			continue
		}
		answer := r.Answer
		r.Answer, r.Held = "", false
		if answer != "" {
			sealed, err := v.Seal([]byte(answer), store.PostLoginAAD(r.ID, h))
			if err != nil {
				return 0, nil, err
			}
			r.Answer = sealed
		}
		d.AddPostLogin(r)
	}

	for _, s := range b.Scripts {
		dup := false
		for _, l := range d.Scripts.Items {
			dup = dup || l.ID == s.ID || (l.Label == s.Label && l.Script == s.Script)
		}
		if !dup {
			d.AddScript(s)
		}
	}
	paths := d.BookmarkPaths()
	for _, p := range b.Bookmarks {
		if !slices.Contains(paths, p) {
			paths = append(paths, p)
		}
	}
	if len(b.Bookmarks) > 0 {
		d.SetBookmarks(paths)
	}
	// A provider's proxy is mapped like a host's; one missing from the
	// backup stays named, so requests fail rather than go direct.
	for _, p := range b.AIProviders {
		if p.LocalOnly() { // a file must not choose what Hostbay runs (KTD7)
			notes = append(notes, "A Claude Code provider in this file was not restored: set it up again on this computer in Settings > AI.")
			continue
		}
		p.ExecPath, p.Consented = "", false
		if p.ID != "" && d.AIProvider(p.ID) != nil {
			continue
		}
		if p.ID == "" {
			p.ID = store.NewID()
		}
		if local := proxyIDs[p.ProxyID]; local != "" {
			p.ProxyID = local
		}
		key := p.APIKey
		p.APIKey = ""
		if key != "" {
			sealed, err := v.Seal([]byte(key), p.SecretAAD())
			if err != nil {
				return 0, nil, err
			}
			p.APIKey = sealed
		}
		d.AddAIProvider(p)
	}
	// The ai group is never restored: a file must not weaken the AI
	// execution mode without the confirmation sync asks for (KTD13).
	for _, g := range b.Settings {
		if g.ID != store.GroupAI && d.SettingsGroup(g.ID) == nil {
			d.AddSettings(g)
		}
	}
	// A pane follows its host to the local copy; one whose host is in
	// neither keeps its ID, and opening the workspace lists it as deleted.
	for _, w := range b.Workspaces {
		if w.ID != "" && d.Workspace(w.ID) != nil {
			continue
		}
		w.Layout = remapPanes(w.Layout, hostIDs)
		d.AddWorkspace(w)
	}
	// A skill whose name is saved here already is skipped: /name keeps
	// attaching the local one.
	for _, sk := range b.Skills {
		if (sk.ID != "" && d.Skill(sk.ID) != nil) || d.SkillNamed(sk.Name) != nil || store.CheckSkill(sk) != nil {
			continue
		}
		d.AddSkill(sk)
	}
	return len(added), notes, nil
}

// matchHost returns the ID of the saved host h matches under KTD12, or "".
// Never by alias alone: SSH Manager allows one alias in several folders.
func matchHost(d *store.Data, h store.Host) string {
	if h.ID != "" && d.Host(h.ID) != nil {
		return h.ID
	}
	for _, l := range d.Hosts.Items {
		if l.Alias == h.Alias && l.Hostname == h.Hostname && l.Port == h.Port && l.User == h.User {
			return l.ID
		}
	}
	return ""
}

// matchProxy returns the ID of the saved proxy p matches: by ID, then by
// type, address and user together; or "".
func matchProxy(d *store.Data, p store.Proxy) string {
	if p.ID != "" && d.Proxy(p.ID) != nil {
		return p.ID
	}
	for _, l := range d.Proxies.Items {
		if l.Type == p.Type && l.Host == p.Host && l.Port == p.Port && l.User == p.User {
			return l.ID
		}
	}
	return ""
}

// remapPanes returns n with each pane's host ID mapped through ids.
func remapPanes(n store.WorkspaceNode, ids map[string]string) store.WorkspaceNode {
	if local, ok := ids[n.HostID]; ok {
		n.HostID = local
	}
	kids := make([]store.WorkspaceNode, len(n.Children))
	for i, c := range n.Children {
		kids[i] = remapPanes(c, ids)
	}
	n.Children = kids
	return n
}

func equalPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }
