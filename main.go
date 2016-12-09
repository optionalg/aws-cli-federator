package main

import (
	"bufio"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/aidan-/aws-cli-federator/federator"
	"github.com/howeyc/gopass"
	"gopkg.in/ini.v1"
)

type configuration struct {
	version *bool
	verbose *bool
	path    string
	cfg     *ini.File

	account string
	profile string
}

var Version = "0.0.1"

var c configuration //arguments
var l *log.Logger

func init() {
	c.version = flag.Bool("version", false, "prints cli version information")
	c.verbose = flag.Bool("v", false, "print debug messages to STDOUT")

	flag.StringVar(&c.path, "path", "", "set path to aws-federator configuration")
	flag.StringVar(&c.account, "account", "", "set which AWS account configuration should be used")
	flag.StringVar(&c.account, "acct", "", "set which AWS account configuration should be used (shorthand)")
	flag.StringVar(&c.profile, "profile", "default", "set which AWS credential profile the temporary credentials should be written to. Defaults to 'default'")

	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", filepath.Base(os.Args[0]))
		flag.PrintDefaults()
		os.Exit(1)
	}
}

func (c *configuration) loadConfigurationFile() error {
	if c.path == "" {
		usr, err := user.Current()
		if err != nil {
			fmt.Printf("Error: Unable to get current user information: %s\n", err)
			os.Exit(1)
		}

		l.Printf("Found user's homedirectory: %s\n", usr.HomeDir)
		c.path = filepath.Join(usr.HomeDir, ".aws/federatedcli")
	}

	l.Printf("Loading configuration from file: %s\n", c.path)
	cfg, err := ini.Load(c.path)
	if err != nil {
		return err
	}
	cfg.BlockMode = false
	c.cfg = cfg

	return nil
}

// findAccount looks through the loaded configuration file to locate a
//   matching account declaration with the account name loaded from the CLI.
// It returns the configuration block if there is a match and false if there
//   is not.
func (c configuration) matchAccount() (*ini.Section, bool) {
	for _, acct := range c.cfg.Sections() {
		if acct.Name() == c.account {
			return acct, true
		}
	}

	return &ini.Section{}, false
}

func main() {
	flag.Parse()

	if *c.version {
		fmt.Fprintf(os.Stderr, "%s version %s\n", filepath.Base(os.Args[0]), Version)
		os.Exit(0)
	}

	l = log.New(ioutil.Discard, "", log.LstdFlags)
	if *c.verbose {
		l.SetOutput(os.Stderr)
	}

	if c.account == "" {
		c.account = "default"
	}

	if err := c.loadConfigurationFile(); err != nil {
		fmt.Printf("Unable to parse configuration file: %s\n", err)
		os.Exit(1)
	}

	acct, found := c.matchAccount()
	if !found {
		fmt.Printf("ERROR: Could not find configuration matching provided account name '%s'\n", c.account)
		os.Exit(1)
	}

	if !acct.HasKey("sp_identity_url") {
		fmt.Printf("ERROR: Account configuration '%s' does not have an 'sp_identity_url' defined\n", c.account)
		os.Exit(1)
	}
	spIdentityURL := acct.Key("sp_identity_url").String()

	//get username
	user := ""
	if acct.HasKey("username") {
		user = acct.Key("username").String()
	} else {
		reader := bufio.NewReader(os.Stdin)
		fmt.Print("Enter Username: ")
		u, _ := reader.ReadString('\n')
		user = strings.TrimSpace(u)
	}

	//get password
	pass := ""
	if acct.HasKey("password") {
		pass = acct.Key("password").String()
	} else {
		fmt.Print("Enter Password: ")
		var err error
		p, err := gopass.GetPasswd()
		if err != nil {
			fmt.Printf("ERROR: Could not get password: %s\n", err)
			os.Exit(1)
		}
		pass = string(p)
		//pass = strings.TrimSpace(p)
	}

	aws, err := federator.New(user, pass, spIdentityURL)
	if err != nil {
		fmt.Printf("ERROR: Failed to initialize federator: %s\n", err)
		os.Exit(1)
	}

	if err = aws.Login(); err != nil {
		fmt.Printf("ERROR: Authentication failure: %s\n", err)
		os.Exit(1)
	}

	roles, err := aws.GetRoles()
	if err != nil {
		fmt.Printf("ERROR: Could not retrieve roles: %s\n", err)
	}

	var roleToAssume federator.Role
	if acct.HasKey("assume_role") {
		for _, r := range roles {
			if acct.Key("assume_role").String() == string(r) {
				roleToAssume = r
				break
			}
		}
		if roleToAssume == "" {
			//couldn't find the role
			fmt.Printf("ERROR: Unable to find role '%s'.  Perhaps your federator configuration is incorrect?\n", acct.Key("assume_role").String())
			os.Exit(1)
		}
	} else {
		if len(roles) == 1 {
			roleToAssume = roles[0]
		} else {
			accountMap, err := c.cfg.GetSection("account_map")
			if err == nil {
				for n, role := range roles {
					if accountMap.HasKey(role.AccountId()) {
						an := accountMap.Key(role.AccountId()).String()
						fmt.Printf("%d) %s:role/%s\n", n+1, an, role.RoleName())
					} else {
						fmt.Printf("%d) %s\n", n+1, role.RoleArn())
					}
				}
			} else {
				for n, role := range roles {
					fmt.Printf("%d) %s\n", n+1, role.RoleArn())
				}
			}
			var i int

			fmt.Printf("Enter the ID# of the role you want to assume: ")

			_, err = fmt.Scanf("%d", &i)
			if err != nil {
				fmt.Printf("ERROR: Invalid selection made.\n")
				os.Exit(1)
			}

			if i > len(roles)+1 {
				fmt.Printf("ERROR: Invalid ID selection, but in range from %d to %d.\n", 1, len(roles)+1)
				os.Exit(1)
			}

			roleToAssume = roles[i-1]
		}
	}

	l.Printf("User has selected ARN: %s\n", roleToAssume)
	l.Printf("Attempting to AssumeRoleWithSAML\n")
	creds, err := aws.AssumeRole(roleToAssume)
	if err != nil {
		fmt.Printf("ERROR: Failed to assume role: %s", err)
		os.Exit(1)
	}

	if err := WriteAWSCredentials(creds, c.profile); err != nil {
		fmt.Printf("ERROR: Failed to write credentials: %s", err)
		os.Exit(1)
	}

	fmt.Println("-------------------------------------------------------\n")
	fmt.Printf("Temporary credentials successfully saved to credential profile '%s'.\nYou can use these credentials with the AWS CLI by including the '--profile %s' flag.\n", c.profile, c.profile)
	fmt.Printf("They will remain valid until %s\n", creds.Expiration.String())
}

func WriteAWSCredentials(c federator.Credentials, p string) error {
	usr, err := user.Current()
	if err != nil {
		return fmt.Errorf("Unable to get current user information: %s\n", err)
	}

	cpath := filepath.Join(usr.HomeDir, ".aws/credentials")

	l.Printf("Writing to AWS credentials file: %s\n", cpath)
	cfg, err := ini.Load(cpath)
	if err != nil {
		return err
	}

	if _, err := cfg.GetSection(p); err != nil {
		if _, err := cfg.NewSection(p); err != nil {
			return fmt.Errorf("Unable to create credential profile: %s", err)
		}
	}

	prof, err := cfg.GetSection(p)
	if err != nil {
		return fmt.Errorf("Unable to retrieve recently created profile: %s", err)
	}

	//aws_access_key_id
	if _, err := prof.NewKey("aws_access_key_id", c.AccessKeyId); err != nil {
		return fmt.Errorf("Unable to write aws_access_key_id to credential file: %s", err)
	}

	//aws_secret_access_key
	if _, err := prof.NewKey("aws_secret_access_key", c.SecretAccessKey); err != nil {
		return fmt.Errorf("Unable to write aws_secret_access_key to credential file: %s", err)
	}

	//aws_session_token
	if _, err := prof.NewKey("aws_session_token", c.SessionToken); err != nil {
		return fmt.Errorf("Unable to write aws_session_token to credential file: %s", err)
	}

	if err := cfg.SaveTo(filepath.Join(usr.HomeDir, ".aws/credentials")); err != nil {
		return fmt.Errorf("Unable to save configuration to disk: %s", err)
	}

	return nil
}