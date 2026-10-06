package config

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// Application contains the small, typed subset of config.yaml needed by the
// Go runtime. More sections can move here as their Python owners are replaced.
type Application struct {
	Path     string
	App      ApplicationApp      `yaml:"app"`
	Security ApplicationSecurity `yaml:"security"`
}

type ApplicationApp struct {
	LoginUser     string `yaml:"login_user"`
	LoginPassword string `yaml:"login_password"`
}

type ApplicationSecurity struct {
	APIKey string `yaml:"api_key"`
}

func LoadApplication(path string) (Application, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Application{}, fmt.Errorf("read application config: %w", err)
	}

	application := Application{Path: path}
	if err := yaml.Unmarshal(contents, &application); err != nil {
		return Application{}, fmt.Errorf("parse application config: %w", err)
	}
	if application.App.LoginUser == "" {
		return Application{}, fmt.Errorf("application config app.login_user is required")
	}
	if application.App.LoginPassword == "" {
		return Application{}, fmt.Errorf("application config app.login_password is required")
	}
	if application.Security.APIKey == "" {
		return Application{}, fmt.Errorf("application config security.api_key is required for native authentication")
	}
	return application, nil
}

func (application Application) DataDirectory() string {
	return filepath.Dir(application.Path)
}

func (application Application) UserDatabasePath() string {
	return filepath.Join(application.DataDirectory(), "user.db")
}
