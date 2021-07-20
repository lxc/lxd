//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"fmt"

	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/shared/api"
)

// Code generation directives.
//
//go:generate -command mapper lxd-generate db mapper -t certificates.mapper.go
//go:generate mapper reset
//
//go:generate mapper stmt -p db -e certificate objects
//go:generate mapper stmt -p db -e certificate projects-ref
//go:generate mapper stmt -p db -e certificate id
//go:generate mapper stmt -p db -e certificate create struct=Certificate
//go:generate mapper stmt -p db -e certificate create-projects-ref
//go:generate mapper stmt -p db -e certificate delete
//go:generate mapper stmt -p db -e certificate update struct=Certificate
//
//go:generate mapper method -p db -e certificate List
//go:generate mapper method -p db -e certificate Get
//go:generate mapper method -p db -e certificate ID struct=Certificate
//go:generate mapper method -p db -e certificate Exists struct=Certificate
//go:generate mapper method -p db -e certificate Create struct=Certificate
//go:generate mapper method -p db -e certificate ProjectsRef
//go:generate mapper method -p db -e certificate DeleteOne
//go:generate mapper method -p db -e certificate DeleteMany
//go:generate mapper method -p db -e certificate Update struct=Certificate

// CertificateType indicates the type of the certificate.
type CertificateType int

// CertificateTypeClient indicates a client certificate type.
const CertificateTypeClient = CertificateType(1)

// CertificateTypeServer indicates a server certificate type.
const CertificateTypeServer = CertificateType(2)

// CertificateAPITypeToDBType converts an API type to the equivalent DB type.
func CertificateAPITypeToDBType(apiType string) (CertificateType, error) {
	switch apiType {
	case api.CertificateTypeClient:
		return CertificateTypeClient, nil
	case api.CertificateTypeServer:
		return CertificateTypeServer, nil
	}

	return -1, fmt.Errorf("Invalid certificate type")
}

// Certificate is here to pass the certificates content from the database around.
type Certificate struct {
	ID          int
	Fingerprint string `db:"primary=yes&comparison=like"`
	Type        CertificateType
	Name        string
	Certificate string
	Restricted  bool
	Projects    []string
}

// ToAPIType returns the API equivalent type.
func (cert *Certificate) ToAPIType() string {
	switch cert.Type {
	case CertificateTypeClient:
		return api.CertificateTypeClient
	case CertificateTypeServer:
		return api.CertificateTypeServer
	}

	return api.CertificateTypeUnknown
}

// ToAPI converts the database Certificate struct to an api.Certificate entry.
func (cert *Certificate) ToAPI() api.Certificate {
	resp := api.Certificate{}
	resp.Fingerprint = cert.Fingerprint
	resp.Certificate = cert.Certificate
	resp.Name = cert.Name
	resp.Restricted = cert.Restricted
	resp.Projects = cert.Projects
	resp.Type = cert.ToAPIType()

	return resp
}

// UpdateCertificateProjects updates the list of projects on a certificate.
func (c *ClusterTx) UpdateCertificateProjects(id int, projects []string) error {
	// Clear all projects from the restrictions.
	q := "DELETE FROM certificates_projects WHERE certificate_id=?"
	_, err := c.tx.Exec(q, id)
	if err != nil {
		return err
	}

	// Add the new restrictions.
	for _, name := range projects {
		projID, err := c.GetProjectID(name)
		if err != nil {
			return err
		}

		q := "INSERT INTO certificates_projects (certificate_id, project_id) VALUES (?, ?)"
		_, err = c.tx.Exec(q, id, projID)
		if err != nil {
			return err
		}
	}

	return nil
}

// CertificateFilter specifies potential query parameter fields.
type CertificateFilter struct {
	Fingerprint string           // Matched with LIKE
	Name        string           `db:"omit=projects-ref"`
	Type        *CertificateType `db:"omit=projects-ref"` // CertificateType is a pointer so it can be omitted.
}

// GetCertificate gets an CertBaseInfo object from the database.
// The argument fingerprint will be queried with a LIKE query, means you can
// pass a shortform and will get the full fingerprint.
// There can never be more than one certificate with a given fingerprint, as it is
// enforced by a UNIQUE constraint in the schema.
func (c *Cluster) GetCertificate(fingerprint string) (*Certificate, error) {
	var err error
	var cert *Certificate
	err = c.Transaction(func(tx *ClusterTx) error {
		cert, err = tx.GetCertificate(fingerprint + "%")
		if err != nil {
			return err
		}

		cert, err = tx.GetCertificate(cert.Fingerprint)
		return err
	})
	if err != nil {
		return nil, err
	}

	return cert, nil
}

// CreateCertificate stores a CertInfo object in the db, it will ignore the ID
// field from the CertInfo.
func (c *Cluster) CreateCertificate(cert Certificate) (int64, error) {
	var id int64
	var err error
	err = c.Transaction(func(tx *ClusterTx) error {
		id, err = tx.CreateCertificate(cert)
		return err
	})
	return id, err
}

// DeleteCertificate deletes a certificate from the db.
func (c *Cluster) DeleteCertificate(fingerprint string) error {
	err := c.Transaction(func(tx *ClusterTx) error {
		return tx.DeleteCertificate(CertificateFilter{Fingerprint: fingerprint})
	})
	return err
}

// UpdateCertificate updates a certificate in the db.
func (c *Cluster) UpdateCertificate(fingerprint string, cert Certificate) error {
	err := c.Transaction(func(tx *ClusterTx) error {
		return tx.UpdateCertificate(fingerprint, cert)
	})
	return err
}

// UpdateCertificateProjects updates the list of projects on a certificate.
func (c *Cluster) UpdateCertificateProjects(id int, projects []string) error {
	err := c.Transaction(func(tx *ClusterTx) error {
		return tx.UpdateCertificateProjects(id, projects)
	})
	return err
}

// GetCertificates returns all available local certificates.
func (n *NodeTx) GetCertificates() ([]Certificate, error) {
	dbCerts := []struct {
		fingerprint string
		certType    CertificateType
		name        string
		certificate string
	}{}
	dest := func(i int) []interface{} {
		dbCerts = append(dbCerts, struct {
			fingerprint string
			certType    CertificateType
			name        string
			certificate string
		}{})
		return []interface{}{&dbCerts[i].fingerprint, &dbCerts[i].certType, &dbCerts[i].name, &dbCerts[i].certificate}
	}

	stmt, err := n.tx.Prepare("SELECT fingerprint, type, name, certificate FROM certificates")
	if err != nil {
		return nil, err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest)
	if err != nil {
		return nil, err
	}

	certs := make([]Certificate, 0, len(dbCerts))
	for _, dbCert := range dbCerts {
		certs = append(certs, Certificate{
			Fingerprint: dbCert.fingerprint,
			Type:        dbCert.certType,
			Name:        dbCert.name,
			Certificate: dbCert.certificate,
		})
	}

	return certs, nil
}

// ReplaceCertificates removes all existing certificates from the local certificates table and replaces them with
// the ones provided.
func (n *NodeTx) ReplaceCertificates(certs []Certificate) error {
	_, err := n.tx.Exec("DELETE FROM certificates")
	if err != nil {
		return err
	}

	stmt, err := n.tx.Prepare("INSERT INTO certificates (fingerprint, type, name, certificate) VALUES(?,?,?,?)")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, cert := range certs {
		_, err = stmt.Exec(cert.Fingerprint, cert.Type, cert.Name, cert.Certificate)
		if err != nil {
			return err
		}
	}

	return nil
}
