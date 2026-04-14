package fiximages

import (
	"errors"
	"fmt"

	"github.com/katiem0/gh-bbc-exporter/internal/data"
	"github.com/katiem0/gh-bbc-exporter/internal/log"
	"github.com/katiem0/gh-bbc-exporter/internal/utils"
	"github.com/spf13/cobra"
	"go.uber.org/zap"
)

type fixImagesFlags struct {
	// Bitbucket source (needed to download images from a private repo)
	BitbucketAccessToken string
	BitbucketAPIToken    string
	BitbucketEmail       string
	BitbucketUser        string
	BitbucketAppPass     string
	BitbucketAPIURL      string
	BitbucketSessionToken string
	Workspace            string
	Repository           string

	// GitHub target
	TargetOrg         string
	TargetRepo        string
	GitHubPAT         string
	TargetAPIURL      string
	GitHubUserSession string

	Debug bool
}

func NewCmdFixImages() *cobra.Command {
	flags := fixImagesFlags{}

	cmd := &cobra.Command{
		Use:   "fix-images [flags]",
		Short: "Re-host Bitbucket inline images to GitHub after migration",
		Long: `Downloads Bitbucket-hosted inline images from migrated GitHub PR descriptions,
uploads them to GitHub's CDN (user-attachments), and patches the PR bodies in place.

Run this after 'migrate' completes.`,
		PreRunE: func(cmd *cobra.Command, args []string) error {
			if flags.Workspace == "" {
				return errors.New("--workspace is required")
			}
			if flags.Repository == "" {
				return errors.New("--repository is required")
			}
			if flags.TargetOrg == "" {
				return errors.New("--target-org is required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			cmd.SilenceUsage = true
			logger, err := log.NewLogger(flags.Debug)
			if err != nil {
				return fmt.Errorf("failed to initialize logger: %w", err)
			}
			defer func() { _ = logger.Sync() }()
			zap.ReplaceGlobals(logger)

			return runFixImages(&flags, logger)
		},
	}

	cmd.Flags().SortFlags = false
	cmd.PersistentFlags().SortFlags = false

	// Bitbucket flags
	cmd.PersistentFlags().StringVarP(&flags.BitbucketAPIURL, "bbc-api-url", "a",
		"https://api.bitbucket.org/2.0", "Bitbucket API URL")
	cmd.PersistentFlags().StringVarP(&flags.BitbucketAccessToken, "access-token", "t", "",
		"Bitbucket workspace access token (env: BITBUCKET_ACCESS_TOKEN)")
	cmd.PersistentFlags().StringVar(&flags.BitbucketAPIToken, "api-token", "",
		"Bitbucket API token (env: BITBUCKET_API_TOKEN)")
	cmd.PersistentFlags().StringVarP(&flags.BitbucketEmail, "email", "e", "",
		"Atlassian account email for API token auth (env: BITBUCKET_EMAIL)")
	cmd.PersistentFlags().StringVarP(&flags.BitbucketUser, "user", "u", "",
		"Bitbucket username (env: BITBUCKET_USERNAME)")
	cmd.PersistentFlags().StringVarP(&flags.BitbucketAppPass, "app-password", "p", "",
		"Bitbucket app password (env: BITBUCKET_APP_PASSWORD)")
	cmd.PersistentFlags().StringVar(&flags.BitbucketSessionToken, "session-token", "",
		"Atlassian cloud.session.token cookie value (copy from browser dev tools)")
	cmd.PersistentFlags().StringVarP(&flags.Workspace, "workspace", "w", "",
		"Bitbucket workspace (required)")
	cmd.PersistentFlags().StringVarP(&flags.Repository, "repository", "r", "",
		"Bitbucket repository slug (required)")

	// GitHub flags
	cmd.PersistentFlags().StringVar(&flags.TargetOrg, "target-org", "",
		"GitHub organization owning the migrated repo (required)")
	cmd.PersistentFlags().StringVar(&flags.TargetRepo, "target-repo", "",
		"GitHub repo name (defaults to --repository if omitted)")
	cmd.PersistentFlags().StringVarP(&flags.GitHubPAT, "github-target-pat", "g", "",
		"GitHub personal access token (env: GH_TOKEN)")
	cmd.PersistentFlags().StringVar(&flags.TargetAPIURL, "target-api-url", "https://api.github.com",
		"GitHub API URL (for GitHub Enterprise)")
	cmd.PersistentFlags().StringVar(&flags.GitHubUserSession, "github-user-session", "",
		"GitHub user_session cookie value for user-attachment uploads (copy from browser dev tools → Application → Cookies → github.com)")

	cmd.PersistentFlags().BoolVarP(&flags.Debug, "debug", "d", false, "Enable debug logging")

	return cmd
}

func runFixImages(flags *fixImagesFlags, logger *zap.Logger) error {
	// Resolve GitHub PAT
	ghToken := flags.GitHubPAT
	if ghToken == "" {
		migrateFlags := &data.CmdMigrateFlags{GitHubPAT: ""}
		var err error
		ghToken, err = utils.GetGitHubAuthToken(migrateFlags, logger)
		if err != nil || ghToken == "" {
			return fmt.Errorf("GitHub token required: set --github-target-pat or GH_TOKEN")
		}
	}

	// Resolve target repo
	targetRepo := flags.TargetRepo
	if targetRepo == "" {
		targetRepo = flags.Repository
	}

	// Build Bitbucket client
	bbClient := utils.NewClient(
		flags.BitbucketAPIURL,
		flags.BitbucketAccessToken,
		flags.BitbucketAPIToken,
		flags.BitbucketEmail,
		flags.BitbucketUser,
		flags.BitbucketAppPass,
		logger,
		"", // no exportDir needed for downloads
		false,
	)

	apiBase := flags.TargetAPIURL
	if apiBase == "" {
		apiBase = "https://api.github.com"
	}

	fixer := utils.NewImageFixer(bbClient, ghToken, apiBase, flags.TargetOrg, targetRepo, flags.BitbucketSessionToken, flags.GitHubUserSession, logger)

	logger.Info("Starting image migration",
		zap.String("bitbucket", fmt.Sprintf("%s/%s", flags.Workspace, flags.Repository)),
		zap.String("github", fmt.Sprintf("%s/%s", flags.TargetOrg, targetRepo)))

	return fixer.FixImages()
}
