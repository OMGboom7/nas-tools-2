package auth

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/0xforee/nas-tools/backend/internal/config"
)

var (
	ErrInvalidCredentials     = errors.New("invalid username or password")
	ErrInvalidUser            = errors.New("invalid user data")
	ErrAdministratorImmutable = errors.New("administrator account is configured in config.yaml")
)

var administratorPermissions = []string{
	"我的媒体库", "资源搜索", "探索", "站点管理", "订阅管理",
	"下载管理", "媒体整理", "服务", "插件", "系统设置",
}

type Service struct {
	mu          sync.RWMutex
	application config.Application
	users       UserStore
	tokens      *TokenManager
}

func NewService(application config.Application) (*Service, error) {
	tokens, err := NewTokenManager(application.Security.APIKey)
	if err != nil {
		return nil, err
	}
	service := &Service{application: application, tokens: tokens}
	store, err := OpenSQLiteUserStore(application.UserDatabasePath())
	if err != nil {
		return nil, err
	}
	service.users = store
	return service, nil
}

func (service *Service) IssueToken(username string, lifetime time.Duration) (string, error) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.tokens.Issue(username, lifetime)
}

func (service *Service) VerifyToken(token string) (Claims, error) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return service.tokens.Verify(token)
}

func (service *Service) RevokeToken(token string) {
	service.mu.RLock()
	defer service.mu.RUnlock()
	service.tokens.Revoke(token)
}

func (service *Service) ReloadApplication(application config.Application) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if application.Security.APIKey != service.application.Security.APIKey {
		tokens, err := NewTokenManager(application.Security.APIKey)
		if err != nil {
			return err
		}
		service.tokens = tokens
	}
	service.application = application
	return nil
}

func (service *Service) Authenticate(ctx context.Context, username, password string) (User, error) {
	service.mu.RLock()
	adminUser := service.application.App.LoginUser
	adminPassword := service.application.App.LoginPassword
	service.mu.RUnlock()
	if username == adminUser && VerifyPassword(adminPassword, password) {
		return User{ID: 0, Name: username, Permissions: append([]string(nil), administratorPermissions...)}, nil
	}
	user, err := service.users.FindByName(ctx, username)
	if err != nil || !VerifyPassword(user.Password, password) {
		return User{}, ErrInvalidCredentials
	}
	if NeedsPasswordRehash(user.Password) {
		if upgraded, hashErr := HashPassword(password); hashErr == nil {
			_ = service.users.UpdatePassword(ctx, user.ID, upgraded)
		}
	}
	return user, nil
}

func (service *Service) FindUser(ctx context.Context, username string) (User, error) {
	service.mu.RLock()
	adminUser := service.application.App.LoginUser
	service.mu.RUnlock()
	if username == adminUser {
		return User{ID: 0, Name: username, Permissions: append([]string(nil), administratorPermissions...)}, nil
	}
	return service.users.FindByName(ctx, username)
}

func (service *Service) ListUsers(ctx context.Context) ([]User, error) {
	return service.users.List(ctx)
}

func (service *Service) CreateUser(ctx context.Context, username, password string, permissions []string) (User, error) {
	username = strings.TrimSpace(username)
	if !validUsername(username) || len(password) < 8 || len(password) > 1024 {
		return User{}, ErrInvalidUser
	}
	if service.IsAdministrator(username) {
		return User{}, ErrUserExists
	}
	permissions, err := normalizePermissions(permissions)
	if err != nil {
		return User{}, err
	}
	hash, err := HashPassword(password)
	if err != nil {
		return User{}, fmt.Errorf("hash new user password: %w", err)
	}
	return service.users.Create(ctx, User{Name: username, Password: hash, Permissions: permissions})
}

func (service *Service) DeleteUser(ctx context.Context, username string) error {
	username = strings.TrimSpace(username)
	if service.IsAdministrator(username) {
		return ErrAdministratorImmutable
	}
	if !validUsername(username) {
		return ErrInvalidUser
	}
	return service.users.Delete(ctx, username)
}

func (service *Service) IsAdministrator(username string) bool {
	service.mu.RLock()
	defer service.mu.RUnlock()
	return username == service.application.App.LoginUser
}

func validUsername(username string) bool {
	if username == "" || len(username) > 128 {
		return false
	}
	for _, character := range username {
		if unicode.IsControl(character) || unicode.IsSpace(character) {
			return false
		}
	}
	return true
}

func normalizePermissions(values []string) ([]string, error) {
	allowed := make(map[string]bool, len(administratorPermissions))
	for _, permission := range administratorPermissions {
		allowed[permission] = true
	}
	seen := make(map[string]bool, len(values))
	permissions := make([]string, 0, len(values))
	for _, value := range values {
		permission := strings.TrimSpace(value)
		if permission == "" || seen[permission] {
			continue
		}
		if !allowed[permission] {
			return nil, ErrInvalidUser
		}
		seen[permission] = true
		permissions = append(permissions, permission)
	}
	return permissions, nil
}
