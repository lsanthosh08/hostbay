import type { SshAlgorithms } from "../api";

// shortcut: a copy of x/crypto/ssh v0.54.0's SupportedAlgorithms() and
// InsecureAlgorithms(), which internal/sshx/algorithms.go validates against;
// no binding exposes them. Expose them from Go when x/crypto next changes.
// Each list is [supported, insecure]; the backend refuses any other name.
export const SSH_ALGORITHMS: Record<keyof SshAlgorithms, [string[], string[]]> = {
  kex: [
    [
      "mlkem768x25519-sha256",
      "curve25519-sha256",
      "ecdh-sha2-nistp256",
      "ecdh-sha2-nistp384",
      "ecdh-sha2-nistp521",
      "diffie-hellman-group14-sha256",
      "diffie-hellman-group16-sha512",
      "diffie-hellman-group-exchange-sha256",
    ],
    ["diffie-hellman-group14-sha1", "diffie-hellman-group1-sha1", "diffie-hellman-group-exchange-sha1"],
  ],
  ciphers: [
    ["aes128-gcm@openssh.com", "aes256-gcm@openssh.com", "chacha20-poly1305@openssh.com", "aes128-ctr", "aes192-ctr", "aes256-ctr"],
    ["aes128-cbc", "3des-cbc", "arcfour256", "arcfour128", "arcfour"],
  ],
  macs: [
    ["hmac-sha2-256-etm@openssh.com", "hmac-sha2-512-etm@openssh.com", "hmac-sha2-256", "hmac-sha2-512", "hmac-sha1"],
    ["hmac-sha1-96"],
  ],
  hostKeys: [
    [
      "rsa-sha2-256-cert-v01@openssh.com",
      "rsa-sha2-512-cert-v01@openssh.com",
      "ecdsa-sha2-nistp256-cert-v01@openssh.com",
      "ecdsa-sha2-nistp384-cert-v01@openssh.com",
      "ecdsa-sha2-nistp521-cert-v01@openssh.com",
      "ssh-ed25519-cert-v01@openssh.com",
      "rsa-sha2-256",
      "rsa-sha2-512",
      "ecdsa-sha2-nistp256",
      "ecdsa-sha2-nistp384",
      "ecdsa-sha2-nistp521",
      "ssh-ed25519",
    ],
    ["ssh-rsa", "ssh-dss", "ssh-rsa-cert-v01@openssh.com", "ssh-dss-cert-v01@openssh.com"],
  ],
};
