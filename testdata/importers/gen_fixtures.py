"""Writes synthetic importer fixtures for Hostbay's testdata/importers.

Run from any directory: python3 -I gen_fixtures.py <path to testdata/importers>

Encrypted values are produced here with Python's `cryptography`, not with
Hostbay's Go decoders, so the Go tests check the published algorithms
rather than their own inverse. Every credential is made up.
"""
import base64
import hashlib
import json
import os
import struct
import sys
import zipfile

from cryptography.hazmat.decrepit.ciphers.algorithms import ARC4, Blowfish
from cryptography.hazmat.decrepit.ciphers.modes import CFB8
from cryptography.hazmat.primitives import hashes
from cryptography.hazmat.primitives.ciphers import Cipher, algorithms, modes
from cryptography.hazmat.primitives.ciphers.aead import AESGCM
from cryptography.hazmat.primitives.kdf.pbkdf2 import PBKDF2HMAC

out = sys.argv[1]
SALT = bytes(range(16))          # fixed so the fixtures are reproducible
NONCE16 = bytes(range(16, 32))
NONCE12 = bytes(range(32, 44))


def write(rel, data):
    p = os.path.join(out, rel)
    os.makedirs(os.path.dirname(p), exist_ok=True)
    with open(p, "wb") as f:
        f.write(data if isinstance(data, bytes) else data.encode())


def pbkdf2(algo, password, salt, rounds, n):
    return PBKDF2HMAC(algorithm=algo, length=n, salt=salt, iterations=rounds).derive(password)


# ---- mRemoteNG: PBKDF2-SHA1 (low byte per UTF-16 unit), AES-256-GCM, 16-byte nonce, salt as AAD
def mrng(plain, master, rounds=1000, salt=SALT, nonce=NONCE16):
    pw = bytes(c & 0xFF for c in master.encode("utf-16-le")[::2])
    key = pbkdf2(hashes.SHA1(), pw, salt, rounds, 32)
    ct = AESGCM(key).encrypt(nonce, plain.encode(), salt)
    return base64.b64encode(salt + nonce + ct).decode()


M = "mr-master"
write("mremoteng/confCons.xml", f"""<?xml version="1.0" encoding="utf-8"?>
<mrng:Connections xmlns:mrng="http://mremoteng.org" Name="Connections" Export="false" EncryptionEngine="AES" BlockCipherMode="GCM" KdfIterations="1000" FullFileEncryption="false" Protected="{mrng('ThisIsProtected', M)}" ConfVersion="2.7">
  <Node Name="Datacenter" Type="Container" Username="ops" Password="{mrng('Ops-Pass-1!', M, salt=bytes(range(1, 17)))}" Protocol="SSH2">
    <Node Name="Linux" Type="Container" InheritUsername="True" InheritPassword="True" InheritProtocol="True">
      <Node Name="db-1" Type="Connection" Hostname="192.0.2.50" Port="22" InheritUsername="True" InheritPassword="True" InheritProtocol="True" PuttySession="Default Settings" />
      <Node Name="old-ssh" Type="Connection" Hostname="192.0.2.51" Protocol="SSH1" Port="22" />
    </Node>
    <Node Name="win-1" Type="Connection" Hostname="198.51.100.10" Protocol="RDP" Port="3390" Username="alice" Domain="CORP" Password="{mrng('Win-Pass-2!', M, salt=bytes(range(2, 18)))}" UseCredSsp="true" RDGatewayUsageMethod="Never" />
    <Node Name="vnc-1" Type="Connection" Hostname="198.51.100.11" Protocol="VNC" Port="5900" Password="{mrng('Vnc-Pass-3!', M, salt=bytes(range(3, 19)))}" />
  </Node>
  <Node Name="router" Type="Connection" Hostname="192.0.2.1" Protocol="Telnet" Port="23" />
  <Node Name="intranet" Type="Connection" Hostname="intranet.example" Protocol="HTTPS" Port="443" />
  <Node Name="evil" Type="Connection" Hostname="192.0.2.99&#10;ProxyCommand touch /tmp/x" Protocol="SSH2" Port="22" />
  <Node Name="raw-svc" Type="Connection" Hostname="192.0.2.60" Protocol="RAW" Port="7000" />
</mrng:Connections>
""")


# ---- MobaXterm: AES-256-CFB8, key SHA-512(master)[:32], IV = AES-ECB(key, 0^16)
def moba(plain, master):
    key = hashlib.sha512(master.encode()).digest()[:32]
    e = Cipher(algorithms.AES(key), modes.ECB()).encryptor()
    iv = e.update(bytes(16)) + e.finalize()
    c = Cipher(algorithms.AES(key), CFB8(iv)).encryptor()
    return base64.b64encode(c.update(plain.encode()) + c.finalize()).decode()


TERM = "#MobaFont%10%0%0%-1%15%236,236,236%30,30,30%180,180,192%0%-1%0%%xterm%-1%-1%_Std_Colors_0_%80%24%0%1%-1%<none>%%0%1%-1%-1#0# #-1"
bookmarks = "\r\n".join([
    "[Bookmarks]",
    "SubRep=",
    "ImgNum=42",
    r"bastion=#109#0%192.0.2.10%22%admin%%-1%-1%%%%%0%0%0%_ProfileDir_\.ssh\id_ed25519%%-1%0%0%0%%1080%%0%0%1" + TERM,
    r"traverse=#109#0%192.0.2.40%22%root%%0%-1%%%%%0%0%0%_ProfileDir_\..\..\etc\shadow%%-1%0%0%0%%1080%%0%0%1" + TERM,
    "evil=#109#0%bad host%22%root%%0%-1%%%%%0%0%0%%%-1%0%0%0%%1080%%0%0%1" + TERM,
    "[Bookmarks_1]",
    r"SubRep=Production\Web",
    "ImgNum=41",
    "web-1=#109#0%192.0.2.21%2222%deploy%%0%-1%%jump.example%22%ops%0%0%0%%%-1%0%0%0%%1080%%0%0%1" + TERM,
    "legacy-switch=#98#1%192.0.2.30%23%%%2%%%%%0%0%%1080%" + TERM,
    r"desktop=#91#4%198.51.100.5%3389%alice%0%0%-1%0%-1%0%-1%0%0%0%%%%0%%0%0%%-1%-1%0" + TERM,
    "console=#128#5%198.51.100.6%5901%-1%0%%%%%0%%0%%%-1" + TERM,
    "files=#130#6%198.51.100.7%21%ftpuser%-1%0%%%" + TERM,
    "uplink=#131#8%COM3%115200%3%0%0%1%2%" + TERM,
    "local=#132#10%0%%%-1%%" + TERM,
    "",
])
write("mobaxterm/sessions.mxtsessions", bookmarks)
write("mobaxterm/MobaXterm.ini", "\r\n".join([
    "[Misc]",
    "SessionP=0123456789",
    "", bookmarks,
    "[Passwords]",
    f"ssh22:admin@192.0.2.10={moba('Moba-Pass-1!', 'moba-master')}",
    f"deploy@192.0.2.21={moba('Moba-Pass-2!', 'moba-master')}",
    "",
]))


# ---- SecureCRT
def crt_v2(plain, passphrase):
    p = plain.encode()
    body = struct.pack("<I", len(p)) + p + hashlib.sha256(p).digest()
    body += bytes(-len(body) % 16)
    c = Cipher(algorithms.AES(hashlib.sha256(passphrase.encode()).digest()), modes.CBC(bytes(16))).encryptor()
    return "02:" + (c.update(body) + c.finalize()).hex()


K1 = bytes.fromhex("24a63dde5bd3b3829c7e06f40816aa07")
K2 = bytes.fromhex("5fb045a29417d916c6c6a2ff064182b7")


def bf(key, data):
    c = Cipher(Blowfish(key), modes.CBC(bytes(8))).encryptor()
    return c.update(data) + c.finalize()


def crt_legacy(plain):
    p = plain.encode("utf-16-le") + b"\0\0"
    p += bytes(-len(p) % 8)
    return "u" + bf(K1, b"\x11\x22\x33\x44" + bf(K2, p) + b"\x55\x66\x77\x88").hex()


def crt(name, items):
    rows = []
    for kind, k, v in items:
        rows.append(f'      <{kind} name="{k}">{v}</{kind}>')
    return f'    <key name="{name}">\n' + "\n".join(rows) + "\n    </key>"


write("securecrt/sessions.xml", "\n".join([
    '<?xml version="1.0" encoding="UTF-8"?>',
    '<VanDyke version="3.0">',
    '  <key name="Sessions">',
    crt("Default", [("string", "Protocol Name", "SSH2"), ("string", "Hostname", "")]),
    '    <key name="Production">',
    crt("app-1", [("string", "Protocol Name", "SSH2"), ("string", "Hostname", "203.0.113.10"), ("dword", "[SSH2] Port", "2200"),
                  ("string", "Username", "deploy"), ("string", "Password V2", crt_v2("Crt-Pass-1!", "crt-pass")),
                  ("dword", "Enable Agent Forwarding", "1"), ("string", "Firewall Name", "None")]),
    crt("app-2", [("string", "Protocol Name", "SSH2"), ("string", "Hostname", "203.0.113.11"), ("string", "Username", "root"),
                  ("string", "Identity Filename V2", "../../etc/ssh/ssh_host_ed25519_key"), ("string", "Firewall Name", "corp-socks"),
                  ("dword", "Forward X11", "1"), ("string", "Password V2", "03:00112233445566778899aabbccddeeff")]),
    "    </key>",
    crt("legacy", [("string", "Protocol Name", "SSH2"), ("string", "Hostname", "203.0.113.12"), ("string", "Username", "admin"),
                   ("string", "Password", crt_legacy("Crt-Old-2!"))]),
    crt("switch", [("string", "Protocol Name", "Telnet"), ("string", "Hostname", "203.0.113.13"), ("dword", "Port", "23")]),
    crt("console", [("string", "Protocol Name", "Serial"), ("string", "Hostname", "")]),
    crt("rlogin-box", [("string", "Protocol Name", "RLogin"), ("string", "Hostname", "203.0.113.14")]),
    "  </key>",
    "</VanDyke>",
    "",
]))


# ---- Xshell: RC4; before 5.1 under MD5("!X@s#h$e%l^l&"), later under SHA-256(master) with SHA-256(plain) appended
def rc4(key, data):
    c = Cipher(ARC4(key), mode=None).encryptor()
    return c.update(data)


def xs_new(plain, master):
    p = plain.encode()
    return base64.b64encode(rc4(hashlib.sha256(master.encode()).digest(), p) + hashlib.sha256(p).digest()).decode()


def xs_old(plain):
    return base64.b64encode(rc4(hashlib.md5(b"!X@s#h$e%l^l&").digest(), plain.encode())).decode()


def xsh(version, proto, host, port, user="", pw="", key="", proxy="", x11="0"):
    text = "\r\n".join([
        "[SessionInfo]", f"Version={version}", "",
        "[CONNECTION]", f"Protocol={proto}", f"Host={host}", f"Port={port}", "",
        "[CONNECTION:AUTHENTICATION]", f"UserName={user}", f"Password={pw}", f"UserKey={key}", "",
        "[CONNECTION:SSH]", f"ForwardX11={x11}", "",
        "[CONNECTION:PROXY]", f"Proxy={proxy}", "",
    ])
    return b"\xff\xfe" + text.encode("utf-16-le")


z = os.path.join(out, "xshell/sessions.xts")
os.makedirs(os.path.dirname(z), exist_ok=True)
with zipfile.ZipFile(z, "w", zipfile.ZIP_DEFLATED) as zf:
    zf.writestr("Xshell/Sessions/Prod/web.xsh", xsh("7.0", "SSH", "203.0.113.20", 22, "deploy", xs_new("Xs-Pass-1!", "xs-master"), x11="1"))
    zf.writestr("Xshell/Sessions/Prod/api.xsh", xsh("7.0", "SSH", "203.0.113.21", 2222, "root", key="id_rsa_2048", proxy="corp-http"))
    zf.writestr("Xshell/Sessions/old.xsh", xsh("5.0", "SSH", "203.0.113.22", 22, "admin", xs_old("Xs-Old-2!")))
    zf.writestr("Xshell/Sessions/tty.xsh", xsh("7.0", "SERIAL", "", 0))
    zf.writestr("Xshell/Sessions/../../escape/switch.xsh", xsh("7.0", "TELNET", "203.0.113.23", 23))
    zf.writestr("Xshell/Sessions/readme.txt", "not a session")


# ---- WindTerm: PBKDF2-HMAC-SHA3-512(master, fingerprint, 100000, 48) -> AES-256-CBC key|IV, PKCS#7
def wt(obj, master, fingerprint):
    m = pbkdf2(hashes.SHA3_512(), master.encode(), fingerprint.encode(), 100000, 48)
    p = json.dumps(obj).encode()
    n = 16 - len(p) % 16
    p += bytes([n]) * n
    c = Cipher(algorithms.AES(m[:32]), modes.CBC(m[32:])).encryptor()
    return base64.b64encode(c.update(p) + c.finalize()).decode()


FP = "synthetic-fp-1234"
write("windterm/profiles/default.v10/user.config", json.dumps({"application.fingerprint": FP, "application.masterPassword": True}, indent=2))
write("windterm/profiles/default.v10/terminal/user.sessions", json.dumps([
    {"session.protocol": "SSH", "session.target": "deploy@203.0.113.30", "session.port": 22, "session.label": "wt-app", "session.group": "Cloud>Prod",
     "session.autoLogin": wt({"PasswordEnabled": True, "Password": "Wt-Pass-1!", "session.user": "deploy"}, "wt-master", FP)},
    {"session.protocol": "SSH", "session.target": "203.0.113.31", "session.label": "wt-key", "session.group": "Cloud",
     "ssh": {"identityFilePath.windows": "$(HomeDir)/.ssh/id_ed25519"}},
    {"session.protocol": "SSH", "session.target": "203.0.113.34", "session.label": "wt-escape",
     "ssh": {"identityFilePath.windows": "$(HomeDir)/../../etc/ssh/ssh_host_rsa_key"}},
    {"session.protocol": "Telnet", "session.target": "203.0.113.32", "session.port": 23, "session.label": "wt-switch"},
    {"session.protocol": "Serial", "session.target": "COM1", "session.label": "wt-serial"},
    {"session.protocol": "SSH", "session.target": "203.0.113.33\nevil", "session.label": "wt-evil"},
], indent=2))


# ---- virt-viewer
write("virtviewer/spice.vv", "\n".join([
    "[virt-viewer]", "type=spice", "host=198.51.100.40", "port=5900", "tls-port=5901", "password=Vv-Ticket-1!",
    "title=vm-guest-1", "ca=-----BEGIN CERTIFICATE-----\\nc3ludGhldGlj\\n-----END CERTIFICATE-----\\n", "delete-this-file=1", ""]))
write("virtviewer/plain-spice.vv", "\n".join([
    "[virt-viewer]", "type=spice", "host=198.51.100.42", "port=5903", "password=Vv-Ticket-2!", "proxy=http://proxy.example:3128", ""]))
write("virtviewer/vnc.vv", "\n".join(["[virt-viewer]", "type=vnc", "host=198.51.100.41", "port=5902", "password=Vv-Vnc-3!", "title=vm-guest-2", ""]))
write("virtviewer/ovirt.vv", "\n".join(["[virt-viewer]", "type=ovirt", "host=198.51.100.43", "title=managed-vm", ""]))
write("virtviewer/evil.vv", "\n".join(["[virt-viewer]", "type=spice", "host=-oProxyCommand=x", "port=5900", "title=evil", ""]))


# ---- uniterm .utm: PBKDF2-HMAC-SHA256(pass, salt, 100000, 32), fields "enc:v1:<nonce b64>:<ct b64>" AES-256-GCM
UK = pbkdf2(hashes.SHA256(), b"utm-pass", SALT, 100000, 32)
_n = [0]


def utm(plain):
    _n[0] += 1
    nonce = bytes([_n[0]]) * 12
    return "enc:v1:" + base64.b64encode(nonce).decode() + ":" + base64.b64encode(AESGCM(UK).encrypt(nonce, plain.encode(), None)).decode()


FAKE_KEY = "-----BEGIN OPENSSH PRIVATE KEY-----\nc3ludGhldGljLWhvc3RiYXktaW1wb3J0LWtleQ==\n-----END OPENSSH PRIVATE KEY-----\n"
write("uniterm/export.utm", json.dumps({
    "format": "uniterm", "version": 1, "encrypted": True,
    "kdf": {"algo": "pbkdf2-sha256", "iterations": 100000, "salt": base64.b64encode(SALT).decode()},
    "groups": [{"id": "grp-1", "name": "Lab"}, {"id": "grp-2", "name": "Edge", "parentId": "grp-1"}, {"id": "grp-3", "name": "Empty"}],
    "connections": [
        {"id": "conn-1", "name": "jump", "type": "ssh", "host": "203.0.113.50", "port": 22, "user": "ops", "authType": "password", "password": utm("Utm-Pass-1!"), "groupId": "grp-1"},
        {"id": "conn-2", "name": "inner", "type": "ssh", "host": "10.0.0.5", "port": 22, "user": "root", "authType": "keyText", "keyContent": utm(FAKE_KEY),
         "tunnelSSHConnId": "conn-1", "agentForwarding": True, "groupId": "grp-2"},
        {"id": "conn-3", "name": "legacy", "type": "ssh", "host": "203.0.113.51", "port": 22, "user": "admin", "authType": "password", "password": utm("Utm-Pass-3!"),
         "sshAlgorithms": {"mode": "custom", "keyExchanges": ["diffie-hellman-group1-sha1"], "ciphers": ["aes128-cbc"]}, "x11Forwarding": True, "proxyId": "proxy-1"},
        {"id": "conn-4", "name": "desk", "type": "rdp", "host": "198.51.100.60", "port": 3390, "user": "alice", "authType": "password", "password": utm("Utm-Rdp-4!"),
         "rdpDomain": "CORP", "rdpEnableNLA": True},
        {"id": "conn-5", "name": "files", "type": "ftp", "host": "198.51.100.61", "port": 21, "user": "ftpuser", "authType": "password", "password": utm("Utm-Ftp-5!"), "ftpEncryption": "none"},
        {"id": "conn-6", "name": "bucket", "type": "s3", "host": "https://minio.example:9000", "port": 0, "user": "AKIAEXAMPLE", "authType": "password",
         "password": utm("Utm-S3-6!"), "s3Bucket": "backups", "s3Region": "us-east-1", "s3UrlStyle": "path"},
        {"id": "conn-7", "name": "dav", "type": "webdav", "host": "https://dav.example/remote.php/webdav", "port": 0, "user": "bob", "authType": "password", "password": utm("Utm-Dav-7!")},
        {"id": "conn-8", "name": "tty", "type": "serial", "host": "", "port": 0, "user": "", "authType": ""},
        {"id": "conn-9", "name": "db", "type": "database", "host": "198.51.100.63", "port": 5432, "user": "pg", "authType": "password"},
        {"id": "conn-10", "name": "evil", "type": "ssh", "host": "203.0.113.52\nProxyCommand x", "port": 22, "user": "root", "authType": "password"},
        {"id": "conn-11", "name": "vm", "type": "vnc", "host": "198.51.100.62", "port": 5900, "user": "", "authType": "password", "password": utm("Utm-Vnc-11!")},
        {"id": "conn-12", "name": "orders", "type": "database", "dbType": "mysql", "host": "10.0.0.7", "port": 3306, "user": "shop", "authType": "password",
         "password": utm("Utm-Db-12!"), "dbName": "orders", "dbParams": "tls=skip-verify&charset=utf8mb4", "tunnelSSHConnId": "conn-1", "groupId": "grp-1"},
        {"id": "conn-13", "name": "search", "type": "elasticsearch", "host": "198.51.100.64", "port": 9200, "user": "elastic", "authType": "password",
         "password": utm("Utm-Es-13!"), "esUseSsl": True, "esSkipVerify": True, "esPathPrefix": "/es"},
        {"id": "conn-14", "name": "sentinel", "type": "redis", "host": "198.51.100.65", "port": 26379, "authType": "password", "redisMode": "sentinel"},
    ],
}, indent=2))


# ---- Navicat .ncx: v12+ AES-128-CBC with the fixed libcckey/libcciv pair, PKCS#7, upper hex;
# v11 Blowfish-ECB keyed by SHA-1("3DC5CA39") in Navicat's XOR feedback mode, after
# github.com/HyperSine/how-does-navicat-encrypt-password. UTF-16LE with a BOM, as Navicat writes it.
def ncx12(plain):
    p = plain.encode()
    p += bytes([16 - len(p) % 16]) * (16 - len(p) % 16)
    c = Cipher(algorithms.AES(b"libcckeylibcckey"), modes.CBC(b"libcciv libcciv ")).encryptor()
    return (c.update(p) + c.finalize()).hex().upper()


def ncx11(plain):
    def enc(b):
        c = Cipher(Blowfish(hashlib.sha1(b"3DC5CA39").digest()), modes.ECB()).encryptor()
        return c.update(b) + c.finalize()
    p, cv, out = plain.encode(), enc(b"\xff" * 8), b""
    full = len(p) // 8 * 8
    for i in range(0, full, 8):
        t = enc(bytes(a ^ b for a, b in zip(p[i:i + 8], cv)))
        cv = bytes(a ^ b for a, b in zip(cv, t))
        out += t
    if full < len(p):
        cv = enc(cv)
        out += bytes(a ^ b for a, b in zip(p[full:], cv))
    return out.hex().upper()


def ncx(name, kind, host, port, folder="", user="", pw="", db="", **extra):
    attrs = {"ConnectionName": name, "ConnType": kind, "Host": host, "Port": port, "Database": db, "UserName": user,
             "Password": pw, "SavePassword": "true" if pw else "false", "Folder": folder, "SSL": "false", "SSH": "false", "HTTP": "false"}
    attrs.update(extra)
    return "  <Connection " + " ".join(f'{k}="{v}"' for k, v in attrs.items()) + " />\n"


write("navicat/connections.ncx", ("﻿" + '<?xml version="1.0" encoding="UTF-16" standalone="yes" ?>\n<Connections Ver="1.5">\n'
    + ncx("shop", "MYSQL", "192.0.2.10", "3306", "Prod", "app", ncx12("Ncx-Mysql-1!"), "shop")
    + ncx("billing", "POSTGRESQL", "192.0.2.11", "5432", "Prod", "pg", ncx12("Ncx-Pg-2!"), "billing", SSL="true", SSL_PGSSLMode="REQUIRE", SSL_WeakCertValidation="true")
    + ncx("erp", "MSSQL", "db1\\SQLEXPRESS", "", "Windows", "sa", ncx12("Ncx-Sql-3!"), "erp")
    + ncx("cache", "REDIS", "192.0.2.13", "6379", "", "", ncx12("Ncx-Redis-4!"), "2")
    + ncx("docs", "MONGODB", "10.0.0.14", "27017", "", "m", ncx12("Ncx-Mongo-5!"), "admin", SSH="true", SSH_Host="bastion.example", SSH_Port="22", SSH_UserName="jump", SSH_Password=ncx12("Ncx-Ssh-6!"))
    + ncx("ledger", "ORACLE", "192.0.2.15", "1521", "Legacy", "scott", ncx11("Ncx-Old-Pass-7!"), "ORCLPDB1")
    + ncx("local", "SQLITE", "", "", "", DatabaseFileName="/srv/data/local.db")
    + ncx("warehouse", "SNOWFLAKE", "acct.example", "443")
    + "</Connections>\n").encode("utf-16-le"))


# ---- DBeaver: credentials-config.json is AES-128-CBC under DBeaver's built-in key, with the
# file's IV as its first 16 bytes. The "master" workspace's file is sealed under another key,
# as one protected by a DBeaver master password reads to Hostbay.
def dbv_creds(obj, key=bytes.fromhex("babb4a9f774ab853c96c2d653dfe544a")):
    iv = bytes(range(48, 64))
    p = json.dumps(obj).encode()
    p += bytes([16 - len(p) % 16]) * (16 - len(p) % 16)
    c = Cipher(algorithms.AES(key), modes.CBC(iv)).encryptor()
    return iv + c.update(p) + c.finalize()


write("dbeaver/workspace/data-sources.json", json.dumps({
    "folders": {"Prod": {}, "Prod/EU": {"parent": "Prod"}},
    "connections": {
        "mysql8-orders": {"provider": "mysql", "driver": "mysql8", "name": "orders", "save-password": True, "folder": "Prod/EU",
                          "configuration": {"host": "192.0.2.20", "port": "3306", "database": "orders", "url": "jdbc:mysql://192.0.2.20:3306/orders",
                                            "bootstrap": {"initQueries": ["SET SESSION sql_mode = 'ANSI'"]}, "local-client": "/usr/local/mysql/bin",
                                            "properties": {"sslMode": "VERIFY_IDENTITY", "connectTimeout": "20000"}}},
        "postgres-jdbc-analytics": {"provider": "postgresql", "driver": "postgres-jdbc", "name": "analytics", "save-password": True, "folder": "Prod",
                                    "configuration": {"host": "192.0.2.21", "port": "5432", "database": "analytics", "properties": {"sslmode": "disable"}}},
        "mssql-erp": {"provider": "sqlserver", "driver": "microsoft", "name": "erp", "save-password": True,
                      "configuration": {"host": "db1\\SQLEXPRESS", "port": "", "database": "erp", "properties": {"encrypt": "true", "trustServerCertificate": "true"}}},
        "postgres-jdbc-tunnelled": {"provider": "postgresql", "name": "tunnelled", "save-password": True,
                                    "configuration": {"host": "10.10.0.5", "port": 5432, "database": "app",
                                                      "handlers": {"ssh_tunnel": {"type": "TUNNEL", "enabled": True, "save-password": True,
                                                                                  "properties": {"host": "bastion.example", "port": 22, "authType": "PASSWORD"}}}}},
        "postgres-jdbc-nonvalidating": {"provider": "postgresql", "name": "nonvalidating",
                                        "configuration": {"host": "192.0.2.23", "handlers": {"postgre_ssl": {"type": "CONFIG", "enabled": True,
                                                          "properties": {"sslMode": "verify-full", "sslFactory": "org.postgresql.ssl.NonValidatingFactory"}}}}},
        "sqlite-local": {"provider": "sqlite", "name": "local file", "configuration": {"database": "/srv/data/app.db"}},
        "by-url": {"provider": "postgresql", "name": "by url", "configuration": {"url": "jdbc:postgresql://192.0.2.25/app?sslmode=disable"}},
        "csv-files": {"provider": "csv", "name": "spreadsheets", "configuration": {"database": "/srv/csv"}},
    },
}, indent=2))
write("dbeaver/workspace/credentials-config.json", dbv_creds({
    "mysql8-orders": {"#connection": {"user": "orders_app", "password": "Dbv-Orders-1!"}},
    "postgres-jdbc-analytics": {"#connection": {"user": "analyst", "password": "Dbv-Pg-2!"}},
    "mssql-erp": {"#connection": {"user": "sa", "password": "Dbv-Sql-3!"}},
    "postgres-jdbc-tunnelled": {"#connection": {"user": "app", "password": "Dbv-Tun-4!"}, "network/ssh_tunnel": {"user": "jump", "password": "Dbv-Ssh-5!"}},
}))
write("dbeaver/master/data-sources.json", json.dumps({"connections": {
    "postgres-jdbc-locked": {"provider": "postgresql", "name": "locked", "save-password": True, "configuration": {"host": "192.0.2.30", "port": "5432", "database": "app"}},
}}, indent=2))
write("dbeaver/master/credentials-config.json", dbv_creds({"postgres-jdbc-locked": {"#connection": {"user": "app", "password": "Dbv-Locked-6!"}}}, key=bytes(range(16))))

# ---- VPN configuration files (U13). Keys are hash-derived filler: random-looking, never real.
def fake_key(label):
    return base64.b64encode(hashlib.sha256(("hostbay-fake-" + label).encode()).digest()).decode()


def fake_pem(kind, marker):
    body = marker + base64.b64encode(hashlib.sha512(marker.encode()).digest() * 2).decode()
    lines = "\n".join(body[i:i + 64] for i in range(0, len(body), 64))
    return f"-----BEGIN {kind}-----\n{lines}\n-----END {kind}-----"


CA = fake_pem("CERTIFICATE", "FakeOvpnCA")
CERT = fake_pem("CERTIFICATE", "FakeOvpnClientCert")
KEY = fake_pem("PRIVATE KEY", "FakeOvpnClientKey")
TLS_AUTH = "-----BEGIN OpenVPN Static key V1-----\n" + hashlib.sha256(b"FakeOvpnTlsAuth").hexdigest() * 2 + "\n-----END OpenVPN Static key V1-----"

write("vpn/office.ovpn", f"""# synthetic split-tunnel profile
client
dev tun
proto udp
remote vpn1.example.test 1195 udp
remote vpn2.example.test 1195 udp
nobind
persist-key
auth-user-pass
remote-cert-tls server
verify-x509-name vpn1.example.test name
cipher AES-256-GCM
route 10.1.0.0 255.255.0.0
route 192.168.40.0 255.255.255.0 vpn_gateway
dhcp-option DNS 10.1.0.53
dhcp-option DOMAIN corp.example.test
tun-mtu 1400
up /tmp/x.sh
script-security 2
plugin /usr/lib/openvpn/plugin-auth-pam.so login
log /tmp/openvpn.log
frobnicate yes
key-direction 1
<ca>
{CA}
</ca>
<cert>
{CERT}
</cert>
<key>
{KEY}
</key>
<tls-auth>
{TLS_AUTH}
</tls-auth>
""")
write("vpn/full.ovpn", """client
dev tun
remote gw.example.test 443 tcp
auth-user-pass /etc/openvpn/creds.txt
redirect-gateway def1
ca /etc/ssl/certs/ca.pem
""")
write("vpn/office.conf", f"""[Interface]
PrivateKey = {fake_key("wg-private")}
Address = 10.8.0.2/32, fd00:8::2/128
DNS = 10.8.0.1, corp.example.test
MTU = 1380
PostUp = iptables -A FORWARD -i %i -j ACCEPT

[Peer]
PublicKey = {fake_key("wg-peer-public")}
PresharedKey = {fake_key("wg-preshared")}
Endpoint = wg.example.test:51821
AllowedIPs = 10.8.0.0/24, 10.20.0.0/16
PersistentKeepalive = 25
""")
write("vpn/anyconnect.xml", """<?xml version="1.0" encoding="UTF-8"?>
<AnyConnectProfile xmlns="http://schemas.xmlsoap.org/encoding/">
  <ClientInitialization><AutoUpdate>false</AutoUpdate></ClientInitialization>
  <ServerList>
    <HostEntry><HostName>HQ</HostName><HostAddress>vpn-hq.example.test</HostAddress><UserGroup>engineering</UserGroup></HostEntry>
    <HostEntry><HostName>Branch</HostName><HostAddress>vpn-branch.example.test:8443</HostAddress></HostEntry>
  </ServerList>
</AnyConnectProfile>
""")
WGQ = f"[Interface]\\nPrivateKey = {fake_key('mc-wg-private')}\\nAddress = 10.9.0.2/32\\n\\n[Peer]\\nPublicKey = {fake_key('mc-wg-peer')}\\nEndpoint = mcwg.example.test:51820\\nAllowedIPs = 10.9.0.0/16\\n"
write("vpn/profile.mobileconfig", f"""<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>PayloadContent</key>
  <array>
    <dict>
      <key>PayloadType</key><string>com.apple.vpn.managed</string>
      <key>UserDefinedName</key><string>Corp IKEv2</string>
      <key>VPNType</key><string>IKEv2</string>
      <key>IKEv2</key>
      <dict>
        <key>RemoteAddress</key><string>ike.example.test</string>
        <key>RemoteIdentifier</key><string>ike.example.test</string>
        <key>AuthenticationMethod</key><string>SharedSecret</string>
        <key>SharedSecret</key><string>Mc-Psk-1!</string>
        <key>ExtendedAuthEnabled</key><integer>1</integer>
        <key>AuthName</key><string>alice</string>
        <key>AuthPassword</key><string>Mc-Pass-2!</string>
      </dict>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.vpn.managed</string>
      <key>UserDefinedName</key><string>Lab WireGuard</string>
      <key>VPNType</key><string>VPN</string>
      <key>VPNSubType</key><string>com.wireguard.macos</string>
      <key>VendorConfig</key>
      <dict><key>WgQuickConfig</key><string>{WGQ.replace(chr(92) + "n", chr(10))}</string></dict>
      <key>VPN</key><dict><key>RemoteAddress</key><string>mcwg.example.test:51820</string></dict>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.vpn.managed</string>
      <key>UserDefinedName</key><string>Vendor App</string>
      <key>VPNType</key><string>VPN</string>
      <key>VPNSubType</key><string>com.example.vendorvpn</string>
      <key>VPN</key><dict><key>RemoteAddress</key><string>vendor.example.test</string></dict>
    </dict>
    <dict>
      <key>PayloadType</key><string>com.apple.wifi.managed</string>
      <key>SSID_STR</key><string>office</string>
    </dict>
  </array>
  <key>PayloadType</key><string>Configuration</string>
</dict>
</plist>
""")
write("vpn/branch.nmconnection", """[connection]
id=Branch office
type=vpn

[vpn]
service-type=org.freedesktop.NetworkManager.fortisslvpn
gateway=ssl.example.test:10443
user=bob
password-flags=1

[ipv4]
method=auto
never-default=true
route1=10.30.0.0/16
dns=10.30.0.53;
dns-search=branch.example.test;
""")
write("vpn/lab.nmconnection", f"""[connection]
id=Lab tunnel
type=wireguard

[wireguard]
private-key={fake_key("nm-wg-private")}

[wireguard-peer.{fake_key("nm-wg-peer")}]
endpoint=nmwg.example.test:51820
allowed-ips=10.40.0.0/16;10.41.0.0/16;

[ipv4]
method=manual
address1=10.40.0.2/32
dns=10.40.0.1;
""")
write("vpn/legacy.nmconnection", """[connection]
id=Legacy IPsec
type=vpn

[vpn]
service-type=org.freedesktop.NetworkManager.vpnc
IPSec gateway=vpnc.example.test
IPSec ID=staff
Xauth username=carol

[vpn-secrets]
IPSec secret=Nm-Group-3!
Xauth password=Nm-Pass-4!

[ipv4]
method=auto
""")
write("vpn/rasphone.pbk", """[Office IKEv2]
Encoding=1
Type=2
PhoneNumber=ike-win.example.test
VpnStrategy=7

[Office L2TP]
Type=2
PhoneNumber=l2tp.example.test
VpnStrategy=3

[Office SSTP]
Type=2
PhoneNumber=sstp.example.test
VpnStrategy=5

[Old PPTP]
Type=2
PhoneNumber=pptp.example.test
VpnStrategy=1

[Dial-up]
Type=1
PhoneNumber=5551234
""")
TB = "Office.tblk/Contents/Resources/"
write("vpn/" + TB + "config.ovpn", """client
dev tun
remote tb.example.test 1194 udp
ca ca.crt
cert client.crt
key client.key
tls-crypt ../../../../outside.key
route 10.50.0.0 255.255.0.0
""")
write("vpn/" + TB + "ca.crt", CA + "\n")
write("vpn/" + TB + "client.crt", CERT + "\n")
write("vpn/" + TB + "client.key", fake_pem("PRIVATE KEY", "FakeTblkClientKey") + "\n")
write("vpn/Home.visc/config.conf", """#-- Config Auto Generated By Viscosity --#
#viscosity name Home Lab
#viscosity autoreconnect true
client
dev tun
remote visc.example.test 1194 udp
auth-user-pass
ca ca.crt
key key.key
redirect-gateway def1
""")
write("vpn/Home.visc/ca.crt", CA + "\n")
write("vpn/Home.visc/key.key", fake_pem("PRIVATE KEY", "FakeViscClientKey") + "\n")
write("tunnelhub/vpn-config.json", json.dumps({
    "vpns": {
        "sslvpn": {"display": "Branch SSL VPN", "match": "Branch", "exclude": "", "iface": "", "peer": "", "kind": "peerless", "socks": "", "mtu": 1280},
        "wg-lab": {"display": "WireGuard Lab", "match": "WireGuard", "exclude": "", "iface": "utun7", "peer": "", "kind": "peer", "socks": "", "mtu": 1380},
        "sase": {"display": "Harmony SASE", "match": "Harmony", "exclude": "Branch", "iface": "", "peer": "100.64.0.0/10", "kind": "peer", "socks": "127.0.0.1:1080", "mtu": 1280},
    },
    "routes": [
        {"dest": "10.212.0.0/16", "vpn": "sslvpn"},
        {"dest": "10.213.0.0/16", "vpn": "sslvpn"},
        {"dest": "10.60.0.0/16", "vpn": "wg-lab"},
        {"dest": "10.61.0.0/16", "vpn": "wg-lab"},
        {"dest": "10.0.0.0/12", "vpn": "sase"},
        {"dest": "172.20.0.0/16", "vpn": "sase"},
        {"dest": "172.21.4.0/24", "vpn": "sase"},
    ],
    "chrome": [{"profile": "Work", "vpn": "sase"}],
}, indent=2))
print("ok")
