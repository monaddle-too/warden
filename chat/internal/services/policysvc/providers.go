package policysvc

import (
	"errors"
	"warden/chat/internal/config"

	"warden/chat/internal/policy"
	kubepolicy "warden/chat/internal/policy/kube"
	"warden/chat/internal/release"
)

// github selects exactly one GitHub credential source from the resolved
// settings: the user token file (local mode, providers.github.authFile or
// --github-auth-file), the user token Secret (the kubernetes kind,
// providers.github.secret, read through store) or the App broker (OVH,
// providers.github.brokerFile or WARDEN_GITHUB_APP_BROKER). Both
// configured is an error; neither leaves GitHub unconfigured.
func (s settings) github(store policy.CredentialStore) (policy.GitHubCredentials, error) {
	switch {
	case s.githubBroker != "" && (s.githubAuthFile != "" || s.githubSecret != ""):
		return nil, errors.New("--github-auth-file and the GitHub App broker are mutually exclusive")
	case s.githubBroker != "":
		app, err := policy.LoadGitHubAppConfig(s.githubBroker, policy.NewRedactor(), nil)
		if err != nil {
			return nil, errors.New("GitHub App broker: " + err.Error())
		}
		return app, nil
	case s.githubSecret != "":
		if store == nil {
			return nil, errors.New("providers.github.secret needs the kubernetes credential store")
		}
		// The Secret may not hold a login yet; every use rereads it and
		// fails closed until it does.
		return policy.NewGitHubUserCredentialsFrom(store, kubepolicy.CredentialName(s.githubSecret, kubepolicy.GitHubAuthKey), policy.NewRedactor(), nil), nil
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
		if s.kind == config.RuntimeKubernetes {
			// The browser reaches chat through the edge only; the callback
			// comes back to the public origin (auth.publicURL).
			options.RedirectBase = s.publicURL
		}
		options.BuiltinClientID = release.GoogleDocsClientID
		options.BuiltinClientSecret = release.GoogleDocsClientSecret
	}
	return policy.NewGoogleConnectionWithClient(s.state, options, nil)
}
