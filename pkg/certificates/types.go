package certificates

// ServiceReference represents a Kubernetes service from webhook clientConfig
type ServiceReference struct {
	Name      string
	Namespace string
	// Hostname is used when webhook uses URL instead of service (for remote clusters)
	// Example: "webhook.example.com" from "https://webhook.example.com:9443/mutate"
	Hostname string
}

// CertificateBundle contains all certificate data
type CertificateBundle struct {
	CACert     []byte
	CAKey      []byte
	ServerCert []byte
	ServerKey  []byte
}
