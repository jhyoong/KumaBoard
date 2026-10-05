package integration

import "crypto/x509"

func poolFrom(pem []byte) *x509.CertPool {
	p := x509.NewCertPool()
	p.AppendCertsFromPEM(pem)
	return p
}
