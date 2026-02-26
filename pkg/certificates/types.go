package certificates

// ServiceReference represents a Kubernetes service from webhook clientConfig
type ServiceReference struct {
	Name      string
	Namespace string
}

// CertificateBundle contains all certificate data
type CertificateBundle struct {
	CACert     []byte
	CAKey      []byte
	ServerCert []byte
	ServerKey  []byte
}
