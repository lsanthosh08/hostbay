package sshx

import (
	"bufio"
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/cipher"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"math/big"
	"strconv"
	"strings"

	"golang.org/x/crypto/argon2"
	"golang.org/x/crypto/ssh"
)

// isPPK reports whether data looks like a PuTTY private key file.
func isPPK(data []byte) bool {
	return bytes.HasPrefix(bytes.TrimLeft(data, "\uFEFF \t\r\n"), []byte("PuTTY-User-Key-File-"))
}

// ParsePPK decodes a PuTTY .ppk private key, versions 2 and 3, unencrypted or
// aes256-cbc encrypted, holding an ssh-rsa or ssh-ed25519 key. It returns an
// *rsa.PrivateKey or ed25519.PrivateKey and the key's comment. An encrypted
// key with no passphrase returns *ssh.PassphraseMissingError; a wrong
// passphrase returns x509.IncorrectPasswordError. The file's MAC is always
// verified.
func ParsePPK(data, passphrase []byte) (crypto.PrivateKey, string, error) {
	f, err := parsePPKFile(data)
	if err != nil {
		return nil, "", err
	}
	encrypted := f.encryption == "aes256-cbc"
	if !encrypted && f.encryption != "none" {
		return nil, "", fmt.Errorf("ppk: unsupported encryption %q", f.encryption)
	}
	if encrypted && len(passphrase) == 0 {
		pub, _ := ssh.ParsePublicKey(f.public)
		return nil, "", &ssh.PassphraseMissingError{PublicKey: pub}
	}
	if !encrypted {
		passphrase = nil
	}

	var cipherKey, iv, macKey []byte
	var newMAC func() hash.Hash
	switch f.version {
	case 2:
		newMAC = sha1.New
		h := sha1.Sum(append([]byte("putty-private-key-file-mac-key"), passphrase...))
		macKey = h[:]
		if encrypted {
			a := sha1.Sum(append([]byte{0, 0, 0, 0}, passphrase...))
			b := sha1.Sum(append([]byte{0, 0, 0, 1}, passphrase...))
			cipherKey, iv = append(a[:], b[:]...)[:32], make([]byte, aes.BlockSize)
		}
	case 3:
		newMAC = sha256.New
		if encrypted {
			out, err := f.argon2(passphrase)
			if err != nil {
				return nil, "", err
			}
			cipherKey, iv, macKey = out[:32], out[32:48], out[48:80]
		}
	default:
		return nil, "", fmt.Errorf("ppk: unsupported version %d", f.version)
	}

	private := f.private
	if encrypted {
		if len(private) == 0 || len(private)%aes.BlockSize != 0 {
			return nil, "", errors.New("ppk: encrypted private blob has a bad length")
		}
		block, _ := aes.NewCipher(cipherKey)
		private = make([]byte, len(f.private))
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(private, f.private)
	}

	mac := hmac.New(newMAC, macKey)
	for _, s := range [][]byte{[]byte(f.algorithm), []byte(f.encryption), []byte(f.comment), f.public, private} {
		var n [4]byte
		binary.BigEndian.PutUint32(n[:], uint32(len(s)))
		mac.Write(n[:])
		mac.Write(s)
	}
	if !hmac.Equal(mac.Sum(nil), f.mac) {
		if encrypted {
			return nil, "", x509.IncorrectPasswordError
		}
		return nil, "", errors.New("ppk: integrity check failed (file is corrupt or was edited)")
	}

	key, err := buildPPKKey(f.algorithm, f.public, private)
	return key, f.comment, err
}

// PPKToPEM converts a .ppk key to an unencrypted OpenSSH PEM private key.
func PPKToPEM(data, passphrase []byte) ([]byte, error) {
	key, comment, err := ParsePPK(data, passphrase)
	if err != nil {
		return nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, comment)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(block), nil
}

type ppkFile struct {
	version                        int
	algorithm, encryption, comment string
	public, private, mac           []byte
	headers                        map[string]string
}

func parsePPKFile(data []byte) (*ppkFile, error) {
	f := &ppkFile{headers: map[string]string{}}
	sc := bufio.NewScanner(bytes.NewReader(data))
	readLines := func(n string) ([]byte, error) {
		count, err := strconv.Atoi(n)
		if err != nil || count < 0 || count > 1024 {
			return nil, errors.New("ppk: bad line count")
		}
		var b strings.Builder
		for range count {
			if !sc.Scan() {
				return nil, errors.New("ppk: file truncated")
			}
			b.WriteString(strings.TrimSpace(sc.Text()))
		}
		return base64.StdEncoding.DecodeString(b.String())
	}
	for sc.Scan() {
		k, v, ok := strings.Cut(strings.TrimSpace(sc.Text()), ": ")
		if !ok {
			continue
		}
		var err error
		switch {
		case strings.HasPrefix(k, "PuTTY-User-Key-File-"):
			f.version, err = strconv.Atoi(strings.TrimPrefix(k, "PuTTY-User-Key-File-"))
			f.algorithm = v
		case k == "Encryption":
			f.encryption = v
		case k == "Comment":
			f.comment = v
		case k == "Public-Lines":
			f.public, err = readLines(v)
		case k == "Private-Lines":
			f.private, err = readLines(v)
		case k == "Private-MAC":
			f.mac, err = hex.DecodeString(v)
		default:
			f.headers[k] = v
		}
		if err != nil {
			return nil, fmt.Errorf("ppk: %s: %w", k, err)
		}
	}
	if f.version == 0 || f.algorithm == "" || f.public == nil || f.private == nil || f.mac == nil {
		return nil, errors.New("not a recognized PPK file")
	}
	return f, nil
}

// argon2 derives the v3 cipher key, IV and MAC key (80 bytes).
func (f *ppkFile) argon2(passphrase []byte) ([]byte, error) {
	num := func(k string, max uint64) (uint64, error) {
		n, err := strconv.ParseUint(f.headers[k], 10, 32)
		if err != nil || n == 0 || n > max {
			return 0, fmt.Errorf("ppk: bad %s", k)
		}
		return n, nil
	}
	mem, err := num("Argon2-Memory", 4<<20) // KiB; caps a hostile file at 4 GiB
	if err != nil {
		return nil, err
	}
	passes, err := num("Argon2-Passes", 1<<16)
	if err != nil {
		return nil, err
	}
	par, err := num("Argon2-Parallelism", 255)
	if err != nil {
		return nil, err
	}
	salt, err := hex.DecodeString(f.headers["Argon2-Salt"])
	if err != nil {
		return nil, errors.New("ppk: bad Argon2-Salt")
	}
	switch f.headers["Key-Derivation"] {
	case "Argon2id":
		return argon2.IDKey(passphrase, salt, uint32(passes), uint32(mem), uint8(par), 80), nil
	case "Argon2i":
		return argon2.Key(passphrase, salt, uint32(passes), uint32(mem), uint8(par), 80), nil
	default:
		return nil, fmt.Errorf("ppk: unsupported key derivation %q", f.headers["Key-Derivation"])
	}
}

func buildPPKKey(algorithm string, public, private []byte) (crypto.PrivateKey, error) {
	pub, priv := wireReader(public), wireReader(private)
	if string(pub.next()) != algorithm {
		return nil, errors.New("ppk: public blob does not match the key type")
	}
	switch algorithm {
	case ssh.KeyAlgoRSA:
		e, n := pub.next(), pub.next()
		d, p, q := priv.next(), priv.next(), priv.next()
		if pub.err != nil || priv.err != nil {
			return nil, errors.New("ppk: RSA key truncated")
		}
		eb := new(big.Int).SetBytes(e)
		if !eb.IsInt64() || eb.Int64() > 1<<31-1 {
			return nil, errors.New("ppk: RSA exponent too large")
		}
		k := &rsa.PrivateKey{
			PublicKey: rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(eb.Int64())},
			D:         new(big.Int).SetBytes(d),
			Primes:    []*big.Int{new(big.Int).SetBytes(p), new(big.Int).SetBytes(q)},
		}
		if err := k.Validate(); err != nil {
			return nil, fmt.Errorf("ppk: invalid RSA key: %w", err)
		}
		k.Precompute()
		return k, nil
	case ssh.KeyAlgoED25519:
		pk, seed := pub.next(), priv.next()
		if pub.err != nil || priv.err != nil || len(pk) != ed25519.PublicKeySize || len(seed) != ed25519.SeedSize {
			return nil, errors.New("ppk: bad ed25519 key length")
		}
		k := ed25519.NewKeyFromSeed(seed)
		if !bytes.Equal(k.Public().(ed25519.PublicKey), pk) {
			return nil, errors.New("ppk: ed25519 private key does not match its public key")
		}
		return k, nil
	default:
		return nil, fmt.Errorf("ppk: unsupported key type %q", algorithm)
	}
}

// wire reads SSH length-prefixed strings; the first short read sets err.
type wire struct {
	buf []byte
	err error
}

func wireReader(b []byte) *wire { return &wire{buf: b} }

func (w *wire) next() []byte {
	if w.err != nil || len(w.buf) < 4 {
		w.err = errors.New("truncated")
		return nil
	}
	n := binary.BigEndian.Uint32(w.buf)
	if uint64(n) > uint64(len(w.buf)-4) {
		w.err = errors.New("truncated")
		return nil
	}
	s := w.buf[4 : 4+n]
	w.buf = w.buf[4+n:]
	return s
}
