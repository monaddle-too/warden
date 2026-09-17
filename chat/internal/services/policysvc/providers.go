package policysvc

import (
	"errors"

	"warden/chat/internal/policy"
	"warden/chat/internal/release"
)

// github selects exactly one GitHub credential source from the resolved
// settings: the user token file (local mode, providers.github.authFile or
// --github-auth-file) or the App broker (OVH, providers.github.brokerFile or
// WARDEN_GITHUB_APP_BROKER). Both configured is an error; neither leaves
// GitHub unconfigured.
func (s settings) github() (policy.GitHubCredentials, error) {
	switch {
	case s.githubBroker != "" && s.githubAuthFile != "":
		return nil, errors.New("--github-auth-file and the GitHub App broker are mutually exclusive")
	case s.githubBroker != "":
		app, err := policy.LoadGitHubAppConfig(s.githubBroker, policy.NewRedactor(), nil)
		if err != nil {
			return nil, errors.New("GitHub App broker: " + err.Error())
		}
		return app, nil
	case s.githubAuthFile != "":
		// The file may not exist yet: the login can run after the service
		// starts, and every use rereads it and fails closed until then.
		return policy.NewGitHubUserCredentials(s.githubAuthFile, policy.NewRedactor(), nil), nil
	}
	return nil, nil
}

// google opens the Docs connection: the operator file when given, else the
// release's built-in Desktop client on the chat port, else unconfigured.
func (s settings) google() (*policy.GoogleConnection, error) {
	options := policy.GoogleClientOptions{ConfigFile: s.googleConfig}
	if s.googleConfigured && s.googleConfig == "" {
		options.ChatListen = s.chatListen
		options.BuiltinClientID = release.GoogleDocsClientID
		options.BuiltinClientSecret = release.GoogleDocsClientSecret
	}
	return policy.NewGoogleConnectionWithClient(s.state, options, nil)
}
