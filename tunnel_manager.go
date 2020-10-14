package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"github.com/caddyserver/certmagic"
	"golang.org/x/crypto/ssh"
	"io/ioutil"
	"log"
	"net"
	"os/user"
	"strconv"
	"strings"
	"sync"
)

type TunnelManager struct {
	config     *BoringProxyConfig
	db         *Database
	mutex      *sync.Mutex
	certConfig *certmagic.Config
	user       *user.User
}

func NewTunnelManager(config *BoringProxyConfig, db *Database, certConfig *certmagic.Config) *TunnelManager {

	user, err := user.Current()
	if err != nil {
		log.Fatalf("Unable to get current user: %v", err)
	}

	for domainName := range db.GetTunnels() {
		err = certConfig.ManageSync([]string{domainName})
		if err != nil {
			log.Println("CertMagic error at startup")
			log.Println(err)
		}
	}

	mutex := &sync.Mutex{}
	return &TunnelManager{config, db, mutex, certConfig, user}
}

func (m *TunnelManager) GetTunnels() map[string]Tunnel {
	return m.db.GetTunnels()
}

func (m *TunnelManager) CreateTunnelForClient(domain, owner, clientName string, clientPort int) (Tunnel, error) {
	tun, err := m.CreateTunnel(domain, owner)
	if err != nil {
		return Tunnel{}, err
	}

	tun.ClientName = clientName
	tun.ClientPort = clientPort

	// TODO: It's a bit hacky that we call db.SetTunnel in CreateTunnel and
	// then again here
	m.db.SetTunnel(domain, tun)

	return tun, nil
}

func (m *TunnelManager) CreateTunnel(domain, owner string) (Tunnel, error) {
	err := m.certConfig.ManageSync([]string{domain})
	if err != nil {
		log.Println(err)
		return Tunnel{}, errors.New("Failed to get cert")
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	_, exists := m.db.GetTunnel(domain)
	if exists {
		return Tunnel{}, errors.New("Tunnel exists for domain " + domain)
	}

	port, err := randomPort()
	if err != nil {
		return Tunnel{}, err
	}

	privKey, err := m.addToAuthorizedKeys(domain, port)
	if err != nil {
		return Tunnel{}, err
	}

	tunnel := Tunnel{
		Owner:            owner,
		ServerAddress:    m.config.WebUiDomain,
		ServerPort:       22,
		ServerPublicKey:  "",
		TunnelPort:       port,
		TunnelPrivateKey: privKey,
		Username:         m.user.Username,
	}

	m.db.SetTunnel(domain, tunnel)

	return tunnel, nil
}

func (m *TunnelManager) DeleteTunnel(domain string) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	tunnel, exists := m.db.GetTunnel(domain)
	if !exists {
		return errors.New("Tunnel doesn't exist")
	}

	m.db.DeleteTunnel(domain)

	authKeysPath := fmt.Sprintf("%s/.ssh/authorized_keys", m.user.HomeDir)

	akBytes, err := ioutil.ReadFile(authKeysPath)
	if err != nil {
		return err
	}

	akStr := string(akBytes)

	lines := strings.Split(akStr, "\n")

	tunnelId := fmt.Sprintf("boringproxy-%s-%d", domain, tunnel.TunnelPort)

	outLines := []string{}

	for _, line := range lines {
		if strings.Contains(line, tunnelId) {
			continue
		}

		outLines = append(outLines, line)
	}

	outStr := strings.Join(outLines, "\n")

	err = ioutil.WriteFile(authKeysPath, []byte(outStr), 0600)
	if err != nil {
		return err
	}

	return nil
}

func (m *TunnelManager) GetPort(domain string) (int, error) {
	tunnel, exists := m.db.GetTunnel(domain)

	if !exists {
		return 0, errors.New("Doesn't exist")
	}

	return tunnel.TunnelPort, nil
}

func (m *TunnelManager) addToAuthorizedKeys(domain string, port int) (string, error) {

	authKeysPath := fmt.Sprintf("%s/.ssh/authorized_keys", m.user.HomeDir)

	akBytes, err := ioutil.ReadFile(authKeysPath)
	if err != nil {
		return "", err
	}

	akStr := string(akBytes)

	pubKey, privKey, err := MakeSSHKeyPair()
	if err != nil {
		return "", err
	}

	options := fmt.Sprintf(`command="echo This key permits tunnels only",permitopen="fakehost:1",permitlisten="127.0.0.1:%d"`, port)

	tunnelId := fmt.Sprintf("boringproxy-%s-%d", domain, port)

	pubKeyNoNewline := pubKey[:len(pubKey)-1]
	newAk := fmt.Sprintf("%s%s %s %s\n", akStr, options, pubKeyNoNewline, tunnelId)
	//newAk := fmt.Sprintf("%s%s %s%d\n", akStr, pubKeyNoNewline, "boringproxy-", port)

	err = ioutil.WriteFile(authKeysPath, []byte(newAk), 0600)
	if err != nil {
		return "", err
	}

	return privKey, nil
}

// Adapted from https://stackoverflow.com/a/34347463/943814
// MakeSSHKeyPair make a pair of public and private keys for SSH access.
// Public key is encoded in the format for inclusion in an OpenSSH authorized_keys file.
// Private Key generated is PEM encoded
func MakeSSHKeyPair() (string, string, error) {
	privateKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		return "", "", err
	}

	// generate and write private key as PEM
	var privKeyBuf strings.Builder

	privateKeyPEM := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(privateKey)}
	if err := pem.Encode(&privKeyBuf, privateKeyPEM); err != nil {
		return "", "", err
	}

	// generate and write public key
	pub, err := ssh.NewPublicKey(&privateKey.PublicKey)
	if err != nil {
		return "", "", err
	}

	pubKey := string(ssh.MarshalAuthorizedKey(pub))

	return pubKey, privKeyBuf.String(), nil
}

func randomPort() (int, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}

	addrParts := strings.Split(listener.Addr().String(), ":")
	port, err := strconv.Atoi(addrParts[len(addrParts)-1])
	if err != nil {
		return 0, err
	}

	listener.Close()

	return port, nil
}
