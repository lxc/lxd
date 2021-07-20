//go:build linux && cgo && !agent
// +build linux,cgo,!agent

package db

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/pkg/errors"

	"github.com/lxc/lxd/lxd/db/query"
	"github.com/lxc/lxd/lxd/instance/instancetype"
	"github.com/lxc/lxd/shared/api"
	"github.com/lxc/lxd/shared/osarch"
)

// Code generation directives.
//
//go:generate -command mapper lxd-generate db mapper -t images.mapper.go
//go:generate mapper reset
//
//go:generate mapper stmt -p db -e image objects
//
//go:generate mapper method -p db -e image List
//go:generate mapper method -p db -e image Get

// Image is a value object holding db-related details about an image.
type Image struct {
	ID           int
	Project      string `db:"primary=yes&join=projects.name"`
	Fingerprint  string `db:"primary=yes&comparison=like"`
	Type         int
	Filename     string
	Size         int64
	Public       bool
	Architecture int
	CreationDate time.Time
	ExpiryDate   time.Time
	UploadDate   time.Time
	Cached       bool
	LastUseDate  time.Time
	AutoUpdate   bool
}

// ImageFilter can be used to filter results yielded by GetImages.
type ImageFilter struct {
	Project     string
	Fingerprint string // Matched with LIKE
	Public      bool
	Cached      bool
	AutoUpdate  bool
}

// ImageSourceProtocol maps image source protocol codes to human-readable names.
var ImageSourceProtocol = map[int]string{
	0: "lxd",
	1: "direct",
	2: "simplestreams",
}

// GetLocalImagesFingerprints returns the fingerprints of all local images.
func (c *ClusterTx) GetLocalImagesFingerprints() ([]string, error) {
	q := `
SELECT images.fingerprint
  FROM images_nodes
  JOIN images ON images.id = images_nodes.image_id
 WHERE node_id = ?
`
	return query.SelectStrings(c.tx, q, c.nodeID)
}

// GetImageSource returns the image source with the given ID.
func (c *ClusterTx) GetImageSource(imageID int) (int, api.ImageSource, error) {
	q := `SELECT id, server, protocol, certificate, alias FROM images_source WHERE image_id=?`
	sources := []struct {
		ID          int
		Server      string
		Protocol    int
		Certificate string
		Alias       string
	}{}
	dest := func(i int) []interface{} {
		sources = append(sources, struct {
			ID          int
			Server      string
			Protocol    int
			Certificate string
			Alias       string
		}{})
		return []interface{}{
			&sources[i].ID,
			&sources[i].Server,
			&sources[i].Protocol,
			&sources[i].Certificate,
			&sources[i].Alias,
		}

	}
	stmt, err := c.tx.Prepare(q)
	if err != nil {
		return -1, api.ImageSource{}, err
	}
	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, imageID)
	if err != nil {
		return -1, api.ImageSource{}, err
	}

	if len(sources) == 0 {
		return -1, api.ImageSource{}, ErrNoSuchObject
	}

	source := sources[0]

	protocol, found := ImageSourceProtocol[source.Protocol]
	if !found {
		return -1, api.ImageSource{}, fmt.Errorf("Invalid protocol: %d", source.Protocol)
	}

	result := api.ImageSource{
		Server:      source.Server,
		Protocol:    protocol,
		Certificate: source.Certificate,
		Alias:       source.Alias,
	}

	return source.ID, result, nil

}

// Fill extra image fields such as properties and alias. This is called after
// fetching a single row from the images table.
func (c *ClusterTx) imageFill(id int, image *api.Image, create, expire, used, upload *time.Time, arch int, imageType int) error {
	// Some of the dates can be nil in the DB, let's process them.
	if create != nil {
		image.CreatedAt = *create
	} else {
		image.CreatedAt = time.Time{}
	}

	if expire != nil {
		image.ExpiresAt = *expire
	} else {
		image.ExpiresAt = time.Time{}
	}

	if used != nil {
		image.LastUsedAt = *used
	} else {
		image.LastUsedAt = time.Time{}
	}

	image.Architecture, _ = osarch.ArchitectureName(arch)
	image.Type = instancetype.Type(imageType).String()

	// The upload date is enforced by NOT NULL in the schema, so it can never be nil.
	image.UploadedAt = *upload

	// Get the properties
	properties, err := query.SelectConfig(c.tx, "images_properties", "image_id=?", id)
	if err != nil {
		return err
	}
	image.Properties = properties

	// Get the aliases
	aliases := []api.ImageAlias{}
	dest := func(i int) []interface{} {
		aliases = append(aliases, api.ImageAlias{})
		return []interface{}{
			&aliases[i].Name,
			&aliases[i].Description,
		}

	}
	q := "SELECT name, description FROM images_aliases WHERE image_id=?"
	stmt, err := c.tx.Prepare(q)
	if err != nil {
		return err
	}

	defer stmt.Close()

	err = query.SelectObjects(stmt, dest, id)
	if err != nil {
		return err
	}

	image.Aliases = aliases

	_, source, err := c.GetImageSource(id)
	if err == nil {
		image.UpdateSource = &source
	}

	return nil
}

func (c *ClusterTx) imageFillProfiles(id int, image *api.Image, project string) error {
	// Check which project name to use
	enabled, err := c.ProjectHasProfiles(project)
	if err != nil {
		return errors.Wrap(err, "Check if project has profiles")
	}
	if !enabled {
		project = "default"
	}

	// Get the profiles
	q := `
SELECT profiles.name FROM profiles
	JOIN images_profiles ON images_profiles.profile_id = profiles.id
	JOIN projects ON profiles.project_id = projects.id
WHERE images_profiles.image_id = ? AND projects.name = ?
`
	profiles, err := query.SelectStrings(c.tx, q, id, project)
	if err != nil {
		return err
	}

	image.Profiles = profiles

	return nil
}

// GetImagesFingerprints returns the names of all images (optionally only the public ones).
func (c *Cluster) GetImagesFingerprints(project string, public bool) ([]string, error) {
	q := `
SELECT fingerprint
  FROM images
  JOIN projects ON projects.id = images.project_id
 WHERE projects.name = ?
`
	if public == true {
		q += " AND public=1"
	}

	var fingerprints []string

	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		fingerprints, err = query.SelectStrings(tx.tx, q, project)
		return err
	})
	if err != nil {
		return nil, err
	}

	return fingerprints, nil
}

// GetExpiredImagesInProject returns the names of all images that have expired since the given time.
func (c *Cluster) GetExpiredImagesInProject(expiry int64, project string) ([]string, error) {
	var images []Image
	err := c.Transaction(func(tx *ClusterTx) error {
		var err error
		images, err = tx.GetImages(ImageFilter{Cached: true, Project: project})
		return err
	})
	if err != nil {
		return nil, err
	}

	results := []string{}
	for _, r := range images {
		// Figure out the expiry
		timestamp := r.UploadDate
		if !r.LastUseDate.IsZero() {
			timestamp = r.LastUseDate
		}

		imageExpiry := timestamp
		imageExpiry = imageExpiry.Add(time.Duration(expiry*24) * time.Hour)

		// Check if expired
		if imageExpiry.After(time.Now()) {
			continue
		}

		results = append(results, r.Fingerprint)
	}

	return results, nil
}

// CreateImageSource inserts a new image source.
func (c *Cluster) CreateImageSource(id int, server string, protocol string, certificate string, alias string) error {
	protocolInt := -1
	for protoInt, protoString := range ImageSourceProtocol {
		if protoString == protocol {
			protocolInt = protoInt
		}
	}

	if protocolInt == -1 {
		return fmt.Errorf("Invalid protocol: %s", protocol)
	}

	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := query.UpsertObject(tx.tx, "images_source", []string{
			"image_id",
			"server",
			"protocol",
			"certificate",
			"alias",
		}, []interface{}{
			id,
			server,
			protocolInt,
			certificate,
			alias,
		})
		return err
	})

	return err
}

// GetCachedImageSourceFingerprint tries to find a source entry of a locally
// cached image that matches the given remote details (server, protocol and
// alias). Return the fingerprint linked to the matching entry, if any.
func (c *Cluster) GetCachedImageSourceFingerprint(server string, protocol string, alias string, typeName string, architecture int) (string, error) {
	imageType := instancetype.Any
	if typeName != "" {
		var err error
		imageType, err = instancetype.New(typeName)
		if err != nil {
			return "", err
		}
	}

	protocolInt := -1
	for protoInt, protoString := range ImageSourceProtocol {
		if protoString == protocol {
			protocolInt = protoInt
		}
	}

	if protocolInt == -1 {
		return "", fmt.Errorf("Invalid protocol: %s", protocol)
	}

	q := `SELECT images.fingerprint
			FROM images_source
			INNER JOIN images
			ON images_source.image_id=images.id
			WHERE server=? AND protocol=? AND alias=? AND auto_update=1 AND images.architecture=?
`

	args := []interface{}{server, protocolInt, alias, architecture}
	if imageType != instancetype.Any {
		q += "AND images.type=?\n"
		args = append(args, imageType)
	}

	q += "ORDER BY creation_date DESC"

	var fingerprints []string
	err := c.Transaction(func(tx *ClusterTx) error {
		var err error
		fingerprints, err = query.SelectStrings(tx.tx, q, args...)
		return err
	})
	if err != nil {
		return "", err
	}
	if len(fingerprints) == 0 {
		return "", ErrNoSuchObject
	}

	return fingerprints[0], nil
}

// ImageExists returns whether an image with the given fingerprint exists.
func (c *Cluster) ImageExists(project string, fingerprint string) (bool, error) {
	table := "images JOIN projects ON projects.id = images.project_id"
	where := "projects.name = ? AND fingerprint=?"

	var exists bool
	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		count, err := query.Count(tx.tx, table, where, project, fingerprint)
		if err != nil {
			return err
		}
		exists = count > 0
		return nil
	})
	if err != nil {
		return false, err
	}

	return exists, nil
}

// ImageIsReferencedByOtherProjects returns true if the image with the given
// fingerprint is referenced by projects other than the given one.
func (c *Cluster) ImageIsReferencedByOtherProjects(project string, fingerprint string) (bool, error) {
	table := "images JOIN projects ON projects.id = images.project_id"
	where := "projects.name != ? AND fingerprint=?"

	var referenced bool
	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		count, err := query.Count(tx.tx, table, where, project, fingerprint)
		if err != nil {
			return err
		}
		referenced = count > 0
		return nil
	})
	if err != nil {
		return false, err
	}

	return referenced, nil
}

// GetImage gets an Image object from the database.
//
// The fingerprint argument will be queried with a LIKE query, means you can
// pass a shortform and will get the full fingerprint. However in case the
// shortform matches more than one image, an error will be returned.
func (c *Cluster) GetImage(project, fingerprint string, public bool) (int, *api.Image, error) {
	var image api.Image
	var object Image
	if fingerprint == "" {
		return -1, nil, ErrNoSuchObject
	}
	err := c.Transaction(func(tx *ClusterTx) error {
		profileProject := project
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		images, err := tx.GetImages(ImageFilter{
			Project:     project,
			Fingerprint: fingerprint + "%",
			Public:      public,
		})
		if err != nil {
			return errors.Wrap(err, "Failed to fetch images")
		}

		switch len(images) {
		case 0:
			return ErrNoSuchObject
		case 1:
			object = images[0]
		default:
			return fmt.Errorf("More than one image matches")
		}

		image.Fingerprint = object.Fingerprint
		image.Filename = object.Filename
		image.Size = object.Size
		image.Cached = object.Cached
		image.Public = object.Public
		image.AutoUpdate = object.AutoUpdate

		err = tx.imageFill(
			object.ID, &image,
			&object.CreationDate, &object.ExpiryDate, &object.LastUseDate,
			&object.UploadDate, object.Architecture, object.Type)
		if err != nil {
			return errors.Wrapf(err, "Fill image details")
		}

		err = tx.imageFillProfiles(object.ID, &image, profileProject)
		if err != nil {
			return errors.Wrapf(err, "Fill image profiles")
		}

		return nil
	})
	if err != nil {
		return -1, nil, err
	}

	return object.ID, &image, nil
}

// GetImageFromAnyProject returns an image matching the given fingerprint, if
// it exists in any project.
func (c *Cluster) GetImageFromAnyProject(fingerprint string) (int, *api.Image, error) {
	// The object we'll actually return
	var image api.Image
	var object Image

	err := c.Transaction(func(tx *ClusterTx) error {
		images, err := tx.GetImages(ImageFilter{
			Fingerprint: fingerprint + "%",
		})
		if err != nil {
			return errors.Wrap(err, "Failed to fetch images")
		}

		if len(images) == 0 {
			return ErrNoSuchObject
		}

		object = images[0]

		image.Fingerprint = object.Fingerprint
		image.Filename = object.Filename
		image.Size = object.Size
		image.Cached = object.Cached
		image.Public = object.Public
		image.AutoUpdate = object.AutoUpdate

		err = tx.imageFill(
			object.ID, &image,
			&object.CreationDate, &object.ExpiryDate, &object.LastUseDate,
			&object.UploadDate, object.Architecture, object.Type)
		if err != nil {
			return errors.Wrapf(err, "Fill image details")
		}

		return nil
	})
	if err != nil {
		return -1, nil, errors.Wrapf(err, "Get image %q", fingerprint)
	}

	return object.ID, &image, nil
}

// LocateImage returns the address of an online node that has a local copy of
// the given image, or an empty string if the image is already available on this
// node.
//
// If the image is not available on any online node, an error is returned.
func (c *Cluster) LocateImage(fingerprint string) (string, error) {
	stmt := `
SELECT nodes.address FROM nodes
  LEFT JOIN images_nodes ON images_nodes.node_id = nodes.id
  LEFT JOIN images ON images_nodes.image_id = images.id
WHERE images.fingerprint = ?
`
	var localAddress string // Address of this node
	var addresses []string  // Addresses of online nodes with the image

	err := c.Transaction(func(tx *ClusterTx) error {
		offlineThreshold, err := tx.GetNodeOfflineThreshold()
		if err != nil {
			return err
		}

		localAddress, err = tx.GetLocalNodeAddress()
		if err != nil {
			return err
		}
		allAddresses, err := query.SelectStrings(tx.tx, stmt, fingerprint)
		if err != nil {
			return err
		}
		for _, address := range allAddresses {
			node, err := tx.GetNodeByAddress(address)
			if err != nil {
				return err
			}
			if address != localAddress && node.IsOffline(offlineThreshold) {
				continue
			}
			addresses = append(addresses, address)
		}
		return err
	})
	if err != nil {
		return "", err
	}
	if len(addresses) == 0 {
		return "", fmt.Errorf("Image not available on any online node")
	}

	for _, address := range addresses {
		if address == localAddress {
			return "", nil
		}
	}

	return addresses[0], nil
}

// AddImageToLocalNode creates a new entry in the images_nodes table for
// tracking that the local node has the given image.
func (c *Cluster) AddImageToLocalNode(project, fingerprint string) error {
	imageID, _, err := c.GetImage(project, fingerprint, false)
	if err != nil {
		return err
	}

	err = c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec("INSERT INTO images_nodes(image_id, node_id) VALUES(?, ?)", imageID, c.nodeID)
		return err
	})
	return err
}

// DeleteImage deletes the image with the given ID.
func (c *Cluster) DeleteImage(id int) error {
	return c.Transaction(func(tx *ClusterTx) error {
		deleted, err := query.DeleteObject(tx.tx, "images", int64(id))
		if err != nil {
			return err
		}
		if !deleted {
			return fmt.Errorf("No image with ID %d", id)
		}
		return nil
	})
}

// GetImageAliases returns the names of the aliases of all images.
func (c *Cluster) GetImageAliases(project string) ([]string, error) {
	var names []string
	q := `
SELECT images_aliases.name
  FROM images_aliases
  JOIN projects ON projects.id=images_aliases.project_id
 WHERE projects.name=?
`

	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		names, err = query.SelectStrings(tx.tx, q, project)
		return err
	})
	if err != nil {
		return nil, err
	}

	return names, nil
}

// GetImageAlias returns the alias with the given name in the given project.
func (c *Cluster) GetImageAlias(project, name string, isTrustedClient bool) (int, api.ImageAliasesEntry, error) {
	id := -1
	entry := api.ImageAliasesEntry{}
	q := `SELECT images_aliases.id, images.fingerprint, images.type, images_aliases.description
			 FROM images_aliases
			 INNER JOIN images
			 ON images_aliases.image_id=images.id
                         INNER JOIN projects
                         ON images_aliases.project_id=projects.id
			 WHERE projects.name=? AND images_aliases.name=?`
	if !isTrustedClient {
		q = q + ` AND images.public=1`
	}

	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}
		var fingerprint, description string
		var imageType int

		arg1 := []interface{}{project, name}
		arg2 := []interface{}{&id, &fingerprint, &imageType, &description}
		err = tx.tx.QueryRow(q, arg1...).Scan(arg2...)
		if err != nil {
			if err == sql.ErrNoRows {
				return ErrNoSuchObject
			}

			return err
		}

		entry.Name = name
		entry.Target = fingerprint
		entry.Description = description
		entry.Type = instancetype.Type(imageType).String()

		return nil
	})
	if err != nil {
		return -1, entry, err
	}

	return id, entry, nil
}

// RenameImageAlias renames the alias with the given ID.
func (c *Cluster) RenameImageAlias(id int, name string) error {
	q := "UPDATE images_aliases SET name=? WHERE id=?"
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(q, name, id)
		return err
	})
	return err
}

// DeleteImageAlias deletes the alias with the given name.
func (c *Cluster) DeleteImageAlias(project, name string) error {
	q := `
DELETE
  FROM images_aliases
 WHERE project_id = (SELECT id FROM projects WHERE name = ?) AND name = ?
`
	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}

		_, err = tx.tx.Exec(q, project, name)
		return err
	})
	if err != nil {
		return err
	}
	return nil
}

// MoveImageAlias changes the image ID associated with an alias.
func (c *Cluster) MoveImageAlias(source int, destination int) error {
	q := "UPDATE images_aliases SET image_id=? WHERE image_id=?"
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(q, destination, source)
		return err
	})
	return err
}

// CreateImageAlias inserts an alias ento the database.
func (c *Cluster) CreateImageAlias(project, name string, imageID int, desc string) error {
	stmt := `
INSERT INTO images_aliases (name, image_id, description, project_id)
     VALUES (?, ?, ?, (SELECT id FROM projects WHERE name = ?))
`
	err := c.Transaction(func(tx *ClusterTx) error {
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}

		_, err = tx.tx.Exec(stmt, name, imageID, desc, project)
		return err
	})
	if err != nil {
		return err
	}

	return nil
}

// UpdateImageAlias updates the alias with the given ID.
func (c *Cluster) UpdateImageAlias(id int, imageID int, desc string) error {
	stmt := `UPDATE images_aliases SET image_id=?, description=? WHERE id=?`
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(stmt, imageID, desc, id)
		return err
	})
	return err
}

// CopyDefaultImageProfiles copies default profiles from id to new_id.
func (c *Cluster) CopyDefaultImageProfiles(id int, newID int) error {
	err := c.Transaction(func(tx *ClusterTx) error {
		// Delete all current associations.
		_, err := tx.tx.Exec("DELETE FROM images_profiles WHERE image_id=?", newID)
		if err != nil {
			return err
		}

		// Copy the entries over.
		_, err = tx.tx.Exec("INSERT INTO images_profiles (image_id, profile_id) SELECT ?, profile_id FROM images_profiles WHERE image_id=?", newID, id)
		if err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return err
	}

	return nil
}

// UpdateImageLastUseDate updates the last_use_date field of the image with the
// given fingerprint.
func (c *Cluster) UpdateImageLastUseDate(fingerprint string, date time.Time) error {
	stmt := `UPDATE images SET last_use_date=? WHERE fingerprint=?`
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(stmt, date, fingerprint)
		return err
	})
	return err
}

// InitImageLastUseDate inits the last_use_date field of the image with the given fingerprint.
func (c *Cluster) InitImageLastUseDate(fingerprint string) error {
	stmt := `UPDATE images SET cached=1, last_use_date=strftime("%s") WHERE fingerprint=?`
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(stmt, fingerprint)
		return err
	})
	return err
}

// UpdateImage updates the image with the given ID.
func (c *Cluster) UpdateImage(id int, fname string, sz int64, public bool, autoUpdate bool, architecture string, createdAt time.Time, expiresAt time.Time, properties map[string]string, project string, profileIds []int64) error {
	arch, err := osarch.ArchitectureId(architecture)
	if err != nil {
		arch = 0
	}

	err = c.Transaction(func(tx *ClusterTx) error {
		publicInt := 0
		if public {
			publicInt = 1
		}

		autoUpdateInt := 0
		if autoUpdate {
			autoUpdateInt = 1
		}

		stmt, err := tx.tx.Prepare(`UPDATE images SET filename=?, size=?, public=?, auto_update=?, architecture=?, creation_date=?, expiry_date=? WHERE id=?`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		_, err = stmt.Exec(fname, sz, publicInt, autoUpdateInt, arch, createdAt, expiresAt, id)
		if err != nil {
			return err
		}

		_, err = tx.tx.Exec(`DELETE FROM images_properties WHERE image_id=?`, id)
		if err != nil {
			return err
		}

		stmt2, err := tx.tx.Prepare(`INSERT INTO images_properties (image_id, type, key, value) VALUES (?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt2.Close()

		for key, value := range properties {
			if value == "" {
				continue
			}

			_, err = stmt2.Exec(id, 0, key, value)
			if err != nil {
				return err
			}
		}

		if project != "" && profileIds != nil {
			enabled, err := tx.ProjectHasProfiles(project)
			if err != nil {
				return err
			}
			if !enabled {
				project = "default"
			}
			q := `DELETE FROM images_profiles
				WHERE image_id = ? AND profile_id IN (
					SELECT profiles.id FROM profiles
					JOIN projects ON profiles.project_id = projects.id
					WHERE projects.name = ?
				)`
			_, err = tx.tx.Exec(q, id, project)
			if err != nil {
				return err
			}

			stmt3, err := tx.tx.Prepare(`INSERT INTO images_profiles (image_id, profile_id) VALUES (?, ?)`)
			if err != nil {
				return err
			}
			defer stmt3.Close()

			for _, profileID := range profileIds {
				_, err = stmt3.Exec(id, profileID)
				if err != nil {
					return err
				}
			}
		}

		return nil
	})
	return err
}

// CreateImage creates a new image.
func (c *Cluster) CreateImage(project, fp string, fname string, sz int64, public bool, autoUpdate bool, architecture string, createdAt time.Time, expiresAt time.Time, properties map[string]string, typeName string) error {
	arch, err := osarch.ArchitectureId(architecture)
	if err != nil {
		arch = 0
	}

	imageType := instancetype.Any
	if typeName != "" {
		var err error
		imageType, err = instancetype.New(typeName)
		if err != nil {
			return err
		}
	}

	if imageType == -1 {
		return fmt.Errorf("Invalid image type: %v", typeName)
	}

	err = c.Transaction(func(tx *ClusterTx) error {
		profileProject := project
		enabled, err := tx.ProjectHasImages(project)
		if err != nil {
			return errors.Wrap(err, "Check if project has images")
		}
		if !enabled {
			project = "default"
		}

		defaultProfileID, _, err := tx.getProfile(profileProject, "default")
		if err != nil {
			return err
		}

		publicInt := 0
		if public {
			publicInt = 1
		}

		autoUpdateInt := 0
		if autoUpdate {
			autoUpdateInt = 1
		}

		stmt, err := tx.tx.Prepare(`INSERT INTO images (project_id, fingerprint, filename, size, public, auto_update, architecture, creation_date, expiry_date, upload_date, type) VALUES ((SELECT id FROM projects WHERE name = ?), ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()

		result, err := stmt.Exec(project, fp, fname, sz, publicInt, autoUpdateInt, arch, createdAt, expiresAt, time.Now().UTC(), imageType)
		if err != nil {
			return err
		}

		id64, err := result.LastInsertId()
		if err != nil {
			return err
		}
		id := int(id64)

		if len(properties) > 0 {
			pstmt, err := tx.tx.Prepare(`INSERT INTO images_properties (image_id, type, key, value) VALUES (?, 0, ?, ?)`)
			if err != nil {
				return err
			}
			defer pstmt.Close()

			for k, v := range properties {
				// we can assume, that there is just one
				// value per key
				_, err = pstmt.Exec(id, k, v)
				if err != nil {
					return err
				}
			}

		}

		_, err = tx.tx.Exec("INSERT INTO images_profiles(image_id, profile_id) VALUES(?, ?)", id, defaultProfileID)
		if err != nil {
			return err
		}

		_, err = tx.tx.Exec("INSERT INTO images_nodes(image_id, node_id) VALUES(?, ?)", id, c.nodeID)
		if err != nil {
			return err
		}

		return nil
	})
	return err
}

// GetPoolsWithImage get the IDs of all storage pools on which a given image exists.
func (c *Cluster) GetPoolsWithImage(imageFingerprint string) ([]int64, error) {
	q := "SELECT storage_pool_id FROM storage_volumes WHERE (node_id=? OR node_id IS NULL) AND name=? AND type=?"
	var ids []int
	err := c.Transaction(func(tx *ClusterTx) error {
		var err error
		ids, err = query.SelectIntegers(tx.tx, q, c.nodeID, imageFingerprint, StoragePoolVolumeTypeImage)
		return err
	})
	if err != nil {
		return nil, err
	}

	poolIDs := make([]int64, len(ids))
	for i, id := range ids {
		poolIDs[i] = int64(id)
	}

	return poolIDs, nil
}

// GetPoolNamesFromIDs get the names of the storage pools with the given IDs.
func (c *Cluster) GetPoolNamesFromIDs(poolIDs []int64) ([]string, error) {
	params := make([]string, len(poolIDs))
	args := make([]interface{}, len(poolIDs))
	for i, id := range poolIDs {
		params[i] = "?"
		args[i] = id
	}
	q := fmt.Sprintf("SELECT name FROM storage_pools WHERE id IN (%s)", strings.Join(params, ","))

	var poolNames []string

	err := c.Transaction(func(tx *ClusterTx) error {
		var err error
		poolNames, err = query.SelectStrings(tx.tx, q, args...)
		return err
	})
	if err != nil {
		return nil, err
	}

	if len(poolNames) != len(poolIDs) {
		return nil, fmt.Errorf("Found only %d matches, expected %d", len(poolNames), len(poolIDs))
	}

	return poolNames, nil
}

// UpdateImageUploadDate updates the upload_date column and an image row.
func (c *Cluster) UpdateImageUploadDate(id int, uploadedAt time.Time) error {
	q := "UPDATE images SET upload_date=? WHERE id=?"
	err := c.Transaction(func(tx *ClusterTx) error {
		_, err := tx.tx.Exec(q, uploadedAt, id)
		return err
	})
	return err
}

// GetImages returns all images.
func (c *Cluster) GetImages() (map[string][]string, error) {
	images := make(map[string][]string) // key is fingerprint, value is list of projects
	err := c.Transaction(func(tx *ClusterTx) error {
		stmt := `
    SELECT images.fingerprint, projects.name FROM images
      LEFT JOIN projects ON images.project_id = projects.id
		`
		rows, err := tx.tx.Query(stmt)
		if err != nil {
			return err
		}

		var fingerprint string
		var projectName string
		for rows.Next() {
			err := rows.Scan(&fingerprint, &projectName)
			if err != nil {
				return err
			}

			images[fingerprint] = append(images[fingerprint], projectName)
		}

		return rows.Err()
	})
	return images, err
}

// GetImagesOnLocalNode returns all images that the local LXD node has.
func (c *Cluster) GetImagesOnLocalNode() (map[string][]string, error) {
	return c.GetImagesOnNode(c.nodeID)
}

// GetImagesOnNode returns all images that the node with the given id has.
func (c *Cluster) GetImagesOnNode(id int64) (map[string][]string, error) {
	images := make(map[string][]string) // key is fingerprint, value is list of projects
	err := c.Transaction(func(tx *ClusterTx) error {
		stmt := `
    SELECT images.fingerprint, projects.name FROM images
      LEFT JOIN images_nodes ON images.id = images_nodes.image_id
			LEFT JOIN nodes ON images_nodes.node_id = nodes.id
			LEFT JOIN projects ON images.project_id = projects.id
    WHERE nodes.id = ?
		`
		rows, err := tx.tx.Query(stmt, id)
		if err != nil {
			return err
		}

		var fingerprint string
		var projectName string
		for rows.Next() {
			err := rows.Scan(&fingerprint, &projectName)
			if err != nil {
				return err
			}

			images[fingerprint] = append(images[fingerprint], projectName)
		}

		return rows.Err()
	})
	return images, err
}

// GetNodesWithImage returns the addresses of online nodes which already have the image.
func (c *Cluster) GetNodesWithImage(fingerprint string) ([]string, error) {
	q := `
SELECT DISTINCT nodes.address FROM nodes
  LEFT JOIN images_nodes ON images_nodes.node_id = nodes.id
  LEFT JOIN images ON images_nodes.image_id = images.id
WHERE images.fingerprint = ?
	`
	return c.getNodesByImageFingerprint(q, fingerprint, nil)
}

// GetNodesWithImageAndAutoUpdate returns the addresses of online nodes which already have the image.
func (c *Cluster) GetNodesWithImageAndAutoUpdate(fingerprint string, autoUpdate bool) ([]string, error) {
	q := `
SELECT DISTINCT nodes.address FROM nodes
  JOIN images_nodes ON images_nodes.node_id = nodes.id
  JOIN images ON images_nodes.image_id = images.id
WHERE images.fingerprint = ? AND images.auto_update = ?
	`
	return c.getNodesByImageFingerprint(q, fingerprint, &autoUpdate)
}

// GetNodesWithoutImage returns the addresses of online nodes which don't have the image.
func (c *Cluster) GetNodesWithoutImage(fingerprint string) ([]string, error) {
	q := `
SELECT DISTINCT nodes.address FROM nodes WHERE nodes.address NOT IN (
  SELECT DISTINCT nodes.address FROM nodes
    LEFT JOIN images_nodes ON images_nodes.node_id = nodes.id
    LEFT JOIN images ON images_nodes.image_id = images.id
  WHERE images.fingerprint = ?)
`
	return c.getNodesByImageFingerprint(q, fingerprint, nil)
}

func (c *Cluster) getNodesByImageFingerprint(stmt, fingerprint string, autoUpdate *bool) ([]string, error) {
	var addresses []string // Addresses of online nodes with the image
	err := c.Transaction(func(tx *ClusterTx) error {
		offlineThreshold, err := tx.GetNodeOfflineThreshold()
		if err != nil {
			return err
		}

		var allAddresses []string

		if autoUpdate == nil {
			allAddresses, err = query.SelectStrings(tx.tx, stmt, fingerprint)
		} else {
			allAddresses, err = query.SelectStrings(tx.tx, stmt, fingerprint, autoUpdate)
		}

		if err != nil {
			return err
		}
		for _, address := range allAddresses {
			node, err := tx.GetNodeByAddress(address)
			if err != nil {
				return err
			}
			if node.IsOffline(offlineThreshold) {
				continue
			}
			addresses = append(addresses, address)
		}
		return err
	})
	return addresses, err
}
