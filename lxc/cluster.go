package main

import (
	"bufio"
	"fmt"
	"io/ioutil"
	"os"
	"sort"
	"strings"

	"github.com/pkg/errors"
	"github.com/spf13/cobra"
	yaml "gopkg.in/yaml.v2"

	"github.com/lxc/lxd/lxc/utils"
	"github.com/lxc/lxd/shared"
	"github.com/lxc/lxd/shared/api"
	cli "github.com/lxc/lxd/shared/cmd"
	"github.com/lxc/lxd/shared/i18n"
	"github.com/lxc/lxd/shared/termios"
)

type cmdCluster struct {
	global *cmdGlobal
}

func (c *cmdCluster) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("cluster")
	cmd.Short = i18n.G("Manage cluster members")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Manage cluster members`))

	// List
	clusterListCmd := cmdClusterList{global: c.global, cluster: c}
	cmd.AddCommand(clusterListCmd.Command())

	// Rename
	clusterRenameCmd := cmdClusterRename{global: c.global, cluster: c}
	cmd.AddCommand(clusterRenameCmd.Command())

	// Remove
	clusterRemoveCmd := cmdClusterRemove{global: c.global, cluster: c}
	cmd.AddCommand(clusterRemoveCmd.Command())

	// Show
	clusterShowCmd := cmdClusterShow{global: c.global, cluster: c}
	cmd.AddCommand(clusterShowCmd.Command())

	// Enable
	clusterEnableCmd := cmdClusterEnable{global: c.global, cluster: c}
	cmd.AddCommand(clusterEnableCmd.Command())

	// Edit
	clusterEditCmd := cmdClusterEdit{global: c.global, cluster: c}
	cmd.AddCommand(clusterEditCmd.Command())

	// Add
	cmdClusterAdd := cmdClusterAdd{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterAdd.Command())

	// List tokens
	cmdClusterListTokens := cmdClusterListTokens{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterListTokens.Command())

	// Revoke tokens
	cmdClusterRevokeToken := cmdClusterRevokeToken{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterRevokeToken.Command())

	// Update certificate
	cmdClusterUpdateCertificate := cmdClusterUpdateCertificate{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterUpdateCertificate.Command())

	// Evacuate cluster member
	cmdClusterEvacuate := cmdClusterEvacuate{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterEvacuate.Command())

	// Restore cluster member
	cmdClusterRestore := cmdClusterRestore{global: c.global, cluster: c}
	cmd.AddCommand(cmdClusterRestore.Command())

	// Workaround for subcommand usage errors. See: https://github.com/spf13/cobra/issues/706
	cmd.Args = cobra.NoArgs
	cmd.Run = func(cmd *cobra.Command, args []string) { cmd.Usage() }

	return cmd
}

// List
type cmdClusterList struct {
	global  *cmdGlobal
	cluster *cmdCluster

	flagFormat string
}

func (c *cmdClusterList) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list", i18n.G("[<remote>:]"))
	cmd.Aliases = []string{"ls"}
	cmd.Short = i18n.G("List all the cluster members")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`List all the cluster members`))
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", "table", i18n.G("Format (csv|json|table|yaml)")+"``")

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterList) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 0, 1)
	if exit {
		return err
	}

	// Parse remote
	remote := ""
	if len(args) == 1 {
		remote = args[0]
	}

	resources, err := c.global.ParseServers(remote)
	if err != nil {
		return err
	}

	resource := resources[0]

	// Check if clustered
	cluster, _, err := resource.server.GetCluster()
	if err != nil {
		return err
	}

	if !cluster.Enabled {
		return fmt.Errorf(i18n.G("LXD server isn't part of a cluster"))
	}

	// Get the cluster members
	members, err := resource.server.GetClusterMembers()
	if err != nil {
		return err
	}

	// Render the table
	data := [][]string{}
	for _, member := range members {
		database := "NO"
		if member.Database {
			database = "YES"
		}
		line := []string{member.ServerName, member.URL, database, member.Architecture, member.FailureDomain, member.Description, strings.ToUpper(member.Status), member.Message}
		data = append(data, line)
	}
	sort.Sort(byName(data))

	header := []string{
		i18n.G("NAME"),
		i18n.G("URL"),
		i18n.G("DATABASE"),
		i18n.G("ARCHITECTURE"),
		i18n.G("FAILURE DOMAIN"),
		i18n.G("DESCRIPTION"),
		i18n.G("STATE"),
		i18n.G("MESSAGE"),
	}

	return utils.RenderTable(c.flagFormat, header, data, members)
}

// Show
type cmdClusterShow struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterShow) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("show", i18n.G("[<remote>:]<member>"))
	cmd.Short = i18n.G("Show details of a cluster member")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Show details of a cluster member`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterShow) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	// Get the member information
	member, _, err := resource.server.GetClusterMember(resource.name)
	if err != nil {
		return err
	}

	// Render as YAML
	data, err := yaml.Marshal(&member)
	if err != nil {
		return err
	}

	fmt.Printf("%s", data)
	return nil
}

// Rename
type cmdClusterRename struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterRename) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("rename", i18n.G("[<remote>:]<member> <new-name>"))
	cmd.Aliases = []string{"mv"}
	cmd.Short = i18n.G("Rename a cluster member")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Rename a cluster member`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterRename) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 2, 2)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	// Perform the rename
	err = resource.server.RenameClusterMember(resource.name, api.ClusterMemberPost{ServerName: args[1]})
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Member %s renamed to %s")+"\n", resource.name, args[1])
	}

	return nil
}

// Remove
type cmdClusterRemove struct {
	global  *cmdGlobal
	cluster *cmdCluster

	flagForce          bool
	flagNonInteractive bool
}

func (c *cmdClusterRemove) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("remove", i18n.G("[<remote>:]<member>"))
	cmd.Aliases = []string{"rm"}
	cmd.Short = i18n.G("Remove a member from the cluster")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Remove a member from the cluster`))

	cmd.RunE = c.Run
	cmd.Flags().BoolVarP(&c.flagForce, "force", "f", false, i18n.G("Force removing a member, even if degraded"))
	cmd.Flags().BoolVarP(&c.flagNonInteractive, "quiet", "q", false, i18n.G("Don't require user confirmation for using --force"))

	return cmd
}

func (c *cmdClusterRemove) promptConfirmation(name string) error {
	reader := bufio.NewReader(os.Stdin)
	fmt.Printf(i18n.G(`Forcefully removing a server from the cluster should only be done as a last
resort.

The removed server will not be functional after this action and will require a
full reset of LXD, losing any remaining instance, image or storage volume
that the server may have held.

When possible, a graceful removal should be preferred, this will require you to
move any affected instance, image or storage volume to another server prior to
the server being cleanly removed from the cluster.

The --force flag should only be used if the server has died, been reinstalled
or is otherwise never expected to come back up.

Are you really sure you want to force removing %s? (yes/no): `), name)
	input, _ := reader.ReadString('\n')
	input = strings.TrimSuffix(input, "\n")

	if !shared.StringInSlice(strings.ToLower(input), []string{i18n.G("yes")}) {
		return fmt.Errorf(i18n.G("User aborted delete operation"))
	}

	return nil
}

func (c *cmdClusterRemove) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	// Prompt for confiromation if --force is used.
	if !c.flagNonInteractive && c.flagForce {
		err := c.promptConfirmation(resource.name)
		if err != nil {
			return err
		}
	}

	// Delete the cluster member
	err = resource.server.DeleteClusterMember(resource.name, c.flagForce)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Member %s removed")+"\n", resource.name)
	}

	return nil
}

// Enable
type cmdClusterEnable struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterEnable) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("enable", i18n.G("[<remote>:] <name>"))
	cmd.Short = i18n.G("Enable clustering on a single non-clustered LXD server")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Enable clustering on a single non-clustered LXD server

  This command turns a non-clustered LXD server into the first member of a new
  LXD cluster, which will have the given name.

  It's required that the LXD is already available on the network. You can check
  that by running 'lxc config get core.https_address', and possibly set a value
  for the address if not yet set.`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterEnable) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 2)
	if exit {
		return err
	}

	// Parse remote
	remote := ""
	name := args[0]
	if len(args) == 2 {
		remote = args[0]
		name = args[1]
	}

	resources, err := c.global.ParseServers(remote)
	if err != nil {
		return err
	}

	resource := resources[0]

	// Check if the LXD server is available on the network.
	server, _, err := resource.server.GetServer()
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve current server config")
	}

	if server.Config["core.https_address"] == "" {
		return fmt.Errorf("This LXD server is not available on the network")
	}

	// Check if already enabled
	currentCluster, etag, err := resource.server.GetCluster()
	if err != nil {
		return errors.Wrap(err, "Failed to retrieve current cluster config")
	}

	if currentCluster.Enabled {
		return fmt.Errorf("This LXD server is already clustered")
	}

	// Enable clustering.
	req := api.ClusterPut{}
	req.ServerName = name
	req.Enabled = true
	op, err := resource.server.UpdateCluster(req, etag)
	if err != nil {
		return errors.Wrap(err, "Failed to configure cluster")
	}

	err = op.Wait()
	if err != nil {
		return errors.Wrap(err, "Failed to configure cluster")
	}

	fmt.Println(i18n.G("Clustering enabled"))
	return nil
}

// Edit
type cmdClusterEdit struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterEdit) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("edit", i18n.G("[<remote>:]<cluster member>"))
	cmd.Short = i18n.G("Edit cluster member configurations as YAML")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(
		`Edit cluster member configurations as YAML`))
	cmd.Example = cli.FormatSection("", i18n.G(
		`lxc cluster edit <cluster member> < member.yaml
    Update a cluster member using the content of member.yaml`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterEdit) helpTemplate() string {
	return i18n.G(
		`### This is a yaml representation of the cluster member.
### Any line starting with a '# will be ignored.`)
}

func (c *cmdClusterEdit) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing cluster member name"))
	}

	// If stdin isn't a terminal, read text from it
	if !termios.IsTerminal(getStdinFd()) {
		contents, err := ioutil.ReadAll(os.Stdin)
		if err != nil {
			return err
		}

		newdata := api.ClusterMemberPut{}
		err = yaml.Unmarshal(contents, &newdata)
		if err != nil {
			return err
		}

		return resource.server.UpdateClusterMember(resource.name, newdata, "")
	}

	// Extract the current value
	member, etag, err := resource.server.GetClusterMember(resource.name)
	if err != nil {
		return err
	}

	memberWritable := member.Writable()

	data, err := yaml.Marshal(&memberWritable)
	if err != nil {
		return err
	}

	// Spawn the editor
	content, err := shared.TextEditor("", []byte(c.helpTemplate()+"\n\n"+string(data)))
	if err != nil {
		return err
	}

	for {
		// Parse the text received from the editor
		newdata := api.ClusterMemberPut{}
		err = yaml.Unmarshal(content, &newdata)
		if err == nil {
			err = resource.server.UpdateClusterMember(resource.name, newdata, etag)
		}

		// Respawn the editor
		if err != nil {
			fmt.Fprintf(os.Stderr, i18n.G("Config parsing error: %s")+"\n", err)
			fmt.Println(i18n.G("Press enter to open the editor again or ctrl+c to abort change"))

			_, err := os.Stdin.Read(make([]byte, 1))
			if err != nil {
				return err
			}

			content, err = shared.TextEditor("", content)
			if err != nil {
				return err
			}
			continue
		}
		break
	}

	return nil
}

func clusterJoinTokenOperationToAPI(op *api.Operation) (*api.ClusterMemberJoinToken, error) {
	serverName, ok := op.Metadata["serverName"].(string)
	if !ok {
		return nil, fmt.Errorf("Operation serverName is type %T not string", op.Metadata["serverName"])
	}

	secret, ok := op.Metadata["secret"].(string)
	if !ok {
		return nil, fmt.Errorf("Operation secret is type %T not string", op.Metadata["secret"])
	}

	fingerprint, ok := op.Metadata["fingerprint"].(string)
	if !ok {
		return nil, fmt.Errorf("Operation fingerprint is type %T not string", op.Metadata["fingerprint"])
	}

	addresses, ok := op.Metadata["addresses"].([]interface{})
	if !ok {
		return nil, fmt.Errorf("Operation addresses is type %T not []interface{}", op.Metadata["addresses"])
	}

	joinToken := api.ClusterMemberJoinToken{
		ServerName:  serverName,
		Secret:      secret,
		Fingerprint: fingerprint,
		Addresses:   make([]string, 0, len(addresses)),
	}

	for i, address := range addresses {
		addressString, ok := address.(string)
		if !ok {
			return nil, fmt.Errorf("Operation address index %d is type %T not string", i, address)
		}

		joinToken.Addresses = append(joinToken.Addresses, addressString)
	}

	return &joinToken, nil
}

// Add
type cmdClusterAdd struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterAdd) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("add", i18n.G("[<remote>:]<member>"))
	cmd.Short = i18n.G("Request a join token for adding a cluster member")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(`Request a join token for adding a cluster member`))

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterAdd) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing cluster member name"))
	}

	// Request the join token.
	member := api.ClusterMembersPost{
		ServerName: resource.name,
	}

	op, err := resource.server.CreateClusterMember(member)
	if err != nil {
		return err
	}

	if !c.global.flagQuiet {
		opAPI := op.Get()
		joinToken, err := clusterJoinTokenOperationToAPI(&opAPI)
		if err != nil {
			return errors.Wrapf(err, "Failed converting token operation to join token")
		}

		fmt.Printf(i18n.G("Member %s join token:")+"\n", resource.name)
		fmt.Println(joinToken.String())
	}

	return nil
}

// List Tokens.
type cmdClusterListTokens struct {
	global  *cmdGlobal
	cluster *cmdCluster

	flagFormat string
}

func (c *cmdClusterListTokens) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("list-tokens", i18n.G("[<remote>:]"))
	cmd.Short = i18n.G("List all active cluster member join tokens")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(`List all active cluster member join tokens`))
	cmd.Flags().StringVarP(&c.flagFormat, "format", "f", "table", i18n.G("Format (csv|json|table|yaml)")+"``")

	cmd.RunE = c.Run

	return cmd
}

func (c *cmdClusterListTokens) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 0, 1)
	if exit {
		return err
	}

	// Parse remote.
	remote := ""
	if len(args) == 1 {
		remote = args[0]
	}

	resources, err := c.global.ParseServers(remote)
	if err != nil {
		return err
	}

	resource := resources[0]

	// Check if clustered.
	cluster, _, err := resource.server.GetCluster()
	if err != nil {
		return err
	}

	if !cluster.Enabled {
		return fmt.Errorf(i18n.G("LXD server isn't part of a cluster"))
	}

	// Get the cluster member join tokens. Use default project as join tokens are created in default project.
	ops, err := resource.server.UseProject("default").GetOperations()
	if err != nil {
		return err
	}

	// Convert the join token operation into encoded form for display.
	type displayToken struct {
		ServerName string
		Token      string
	}

	displayTokens := make([]displayToken, 0)

	for _, op := range ops {
		if op.Class != api.OperationClassToken {
			continue
		}

		if op.StatusCode != api.Running {
			continue // Tokens are single use, so if cancelled but not deleted yet its not available.
		}

		joinToken, err := clusterJoinTokenOperationToAPI(&op)
		if err != nil {
			continue // Operation is not a valid cluster member join token operation.
		}

		displayTokens = append(displayTokens, displayToken{
			ServerName: joinToken.ServerName,
			Token:      joinToken.String(),
		})
	}

	// Render the table.
	data := [][]string{}
	for _, token := range displayTokens {
		line := []string{token.ServerName, token.Token}
		data = append(data, line)
	}
	sort.Sort(byName(data))

	header := []string{
		i18n.G("NAME"),
		i18n.G("TOKEN"),
	}

	return utils.RenderTable(c.flagFormat, header, data, displayTokens)
}

// Revoke Tokens.
type cmdClusterRevokeToken struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterRevokeToken) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("revoke-token", i18n.G("[<remote>:]<token>"))
	cmd.Short = i18n.G("Revoke cluster member join token")
	cmd.Long = cli.FormatSection(i18n.G("Description"), cmd.Short)

	cmd.RunE = c.Run
	return cmd
}

func (c *cmdClusterRevokeToken) Run(cmd *cobra.Command, args []string) error {
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return err
	}

	resource := resources[0]

	// Check if clustered.
	cluster, _, err := resource.server.GetCluster()
	if err != nil {
		return err
	}

	if !cluster.Enabled {
		return fmt.Errorf(i18n.G("LXD server isn't part of a cluster"))
	}

	// Get the cluster member join tokens. Use default project as join tokens are created in default project.
	ops, err := resource.server.UseProject("default").GetOperations()
	if err != nil {
		return err
	}

	for _, op := range ops {
		if op.Class != api.OperationClassToken {
			continue
		}

		if op.StatusCode != api.Running {
			continue // Tokens are single use, so if cancelled but not deleted yet its not available.
		}

		joinToken, err := clusterJoinTokenOperationToAPI(&op)
		if err != nil {
			continue // Operation is not a valid cluster member join token operation.
		}

		if joinToken.ServerName == resource.name {
			// Delete the operation
			err = resource.server.DeleteOperation(op.ID)
			if err != nil {
				return err
			}

			if !c.global.flagQuiet {
				fmt.Printf(i18n.G("Cluster join token for %s:%s deleted")+"\n", resource.remote, resource.name)
			}

			return nil
		}
	}

	return fmt.Errorf(i18n.G("No cluster join token for member %s on remote: %s"), resource.name, resource.remote)
}

// Update Certificatess
type cmdClusterUpdateCertificate struct {
	global  *cmdGlobal
	cluster *cmdCluster
}

func (c *cmdClusterUpdateCertificate) Command() *cobra.Command {
	cmd := &cobra.Command{}
	cmd.Use = usage("update-certificate", i18n.G("[<remote>:] <cert.crt> <cert.key>"))
	cmd.Aliases = []string{"update-cert"}
	cmd.Short = i18n.G("Update cluster certificate")
	cmd.Long = cli.FormatSection(i18n.G("Description"),
		i18n.G("Update cluster certificate with PEM certificate and key read from input files."))

	cmd.RunE = c.Run
	return cmd
}

func (c *cmdClusterUpdateCertificate) Run(cmd *cobra.Command, args []string) error {
	conf := c.global.conf

	exit, err := c.global.CheckArgs(cmd, args, 2, 3)
	if exit {
		return err
	}

	// Parse remote
	remote := ""
	certFile := args[0]
	keyFile := args[1]
	if len(args) == 3 {
		remote = args[0]
		certFile = args[1]
		keyFile = args[2]
	}

	resources, err := c.global.ParseServers(remote)
	if err != nil {
		return err
	}

	resource := resources[0]

	// Check if clustered.
	cluster, _, err := resource.server.GetCluster()
	if err != nil {
		return err
	}

	if !cluster.Enabled {
		return fmt.Errorf(i18n.G("LXD server isn't part of a cluster"))
	}

	if !shared.PathExists(certFile) {
		return fmt.Errorf(i18n.G("Could not find certificate file path: %s"), certFile)
	}

	if !shared.PathExists(keyFile) {
		return fmt.Errorf(i18n.G("Could not find certificate key file path: %s"), keyFile)
	}

	cert, err := ioutil.ReadFile(certFile)
	if err != nil {
		return fmt.Errorf(i18n.G("Could not read certificate file: %s with error: %v"), certFile, err)
	}

	key, err := ioutil.ReadFile(keyFile)
	if err != nil {
		return fmt.Errorf(i18n.G("Could not read certificate key file: %s with error: %v"), keyFile, err)
	}

	certificates := api.ClusterCertificatePut{
		ClusterCertificate:    string(cert),
		ClusterCertificateKey: string(key),
	}

	err = resource.server.UpdateClusterCertificate(certificates, "")
	if err != nil {
		return err
	}

	certf := conf.ServerCertPath(resource.remote)
	if shared.PathExists(certf) {
		err = ioutil.WriteFile(certf, cert, 0644)
		if err != nil {
			return fmt.Errorf(i18n.G("Could not write new remote certificate for remote '%s' with error: %v"), resource.remote, err)
		}
	}

	if !c.global.flagQuiet {
		fmt.Printf(i18n.G("Successfully updated cluster certificates for remote %s")+"\n", resource.remote)
	}

	return nil
}

type cmdClusterEvacuateAction struct {
	global *cmdGlobal

	flagForce bool
}

// Cluster member evacuation
type cmdClusterEvacuate struct {
	global  *cmdGlobal
	cluster *cmdCluster
	action  *cmdClusterEvacuateAction
}

func (c *cmdClusterEvacuate) Command() *cobra.Command {
	cmdAction := cmdClusterEvacuateAction{global: c.global}
	c.action = &cmdAction

	cmd := c.action.Command("evacuate")
	cmd.Aliases = []string{"evac"}
	cmd.Use = usage("evacuate", i18n.G("[<remote>:]<member>"))
	cmd.Short = i18n.G("Evacuate cluster member")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(`Evacuate cluster member`))

	return cmd
}

// Cluster member restore
type cmdClusterRestore struct {
	global  *cmdGlobal
	cluster *cmdCluster
	action  *cmdClusterEvacuateAction
}

func (c *cmdClusterRestore) Command() *cobra.Command {
	cmdAction := cmdClusterEvacuateAction{global: c.global}
	c.action = &cmdAction

	cmd := c.action.Command("restore")
	cmd.Use = usage("restore", i18n.G("[<remote>:]<member>"))
	cmd.Short = i18n.G("Restore cluster member")
	cmd.Long = cli.FormatSection(i18n.G("Description"), i18n.G(`Restore cluster member`))

	return cmd
}

func (c *cmdClusterEvacuateAction) Command(action string) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.RunE = c.Run
	cmd.Flags().BoolVar(&c.flagForce, "force", false, i18n.G(`Force evacuation without user confirmation`)+"``")

	return cmd
}

func (c *cmdClusterEvacuateAction) Run(cmd *cobra.Command, args []string) error {
	// Quick checks.
	exit, err := c.global.CheckArgs(cmd, args, 1, 1)
	if exit {
		return err
	}

	// Parse remote.
	resources, err := c.global.ParseServers(args[0])
	if err != nil {
		return errors.Wrap(err, "Failed to parse servers")
	}

	resource := resources[0]

	if resource.name == "" {
		return fmt.Errorf(i18n.G("Missing cluster member name"))
	}

	if !c.flagForce && !cli.AskBool(fmt.Sprintf(i18n.G("Are you sure you want to %s cluster member %q? (yes/no) [default=no]: "), cmd.Name(), resource.name), "no") {
		return nil
	}

	state := api.ClusterMemberStatePost{
		Action: cmd.Name(),
	}

	op, err := resource.server.UpdateClusterMemberState(resource.name, state)
	if err != nil {
		return errors.Wrap(err, "Failed to update cluster member state")
	}

	var format string

	if cmd.Name() == "restore" {
		format = i18n.G("Restoring cluster member: %s")
	} else {
		format = i18n.G("Evacuating cluster member: %s")
	}

	progress := utils.ProgressRenderer{
		Format: format,
		Quiet:  c.global.flagQuiet,
	}

	_, err = op.AddHandler(progress.UpdateOp)
	if err != nil {
		progress.Done("")
		return err
	}

	err = op.Wait()
	if err != nil {
		progress.Done("")
		return err
	}

	progress.Done("")
	return nil
}
