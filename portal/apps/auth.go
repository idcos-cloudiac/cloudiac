// Copyright (c) 2015-2022 CloudJ Technology Co., Ltd.

package apps

import (
	"cloudiac/configs"
	"cloudiac/portal/consts/e"
	"cloudiac/portal/libs/ctx"
	"cloudiac/portal/models"
	"cloudiac/portal/models/forms"
	"cloudiac/portal/models/resps"
	"cloudiac/portal/services"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Login 用户登陆
func Login(c *ctx.ServiceContext, form *forms.LoginForm) (resp interface{}, er e.Error) {
	c.AddLogField("action", fmt.Sprintf("user login: %s", form.Email))
	user, er := services.GetUserByEmail(c.DB(), form.Email)
	loginSucceed := false
	localUserNotExists := false
	if er != nil {
		// 当错误为用户邮箱不存在的时候，尝试使用ldap 进行登录
		if er.Code() == e.UserNotExists {
			localUserNotExists = true
		} else {
			return nil, er
		}
	} else {
		if valid, err := services.VerifyLocalPassword(user, form.Password); err != nil {
			return nil, e.New(e.InternalError, http.StatusInternalServerError)
		} else if valid {
			loginSucceed = true
		}
	}

	if !loginSucceed && configs.Get().Ldap.LdapServer != "" { // 本地登录失败，尝试 ldap 登录
		username, _, er := services.VerifyLdapPassword(form.Email, form.Password)
		if er != nil {
			return nil, er
		}

		loginSucceed = true
		if localUserNotExists {
			// ldap 登录成功, 在用户表中添加该用户
			if user, er = createLdapUser(c, username, form.Email); er != nil {
				c.Logger().Warnf("create ldap user error: %v", er)
				return nil, er
			}
		}
	}

	if !loginSucceed {
		return nil, e.New(e.InvalidPassword)
	}

	dn, er := services.QueryLdapUserDN(user.Email)
	if er != nil {
		c.Logger().Debugf("query user dn error: %v", er)
		return nil, er
	}

	// 刷新用户权限
	if er := refreshLdapUserRole(c, user, dn); er != nil {
		c.Logger().Warnf("refresh user role error: %v", er)
		return nil, er
	}

	token, err := services.GenerateToken(user.Id, user.Name, user.IsAdmin, 1*24*time.Hour)
	if err != nil {
		c.Logger().Errorf("name [%s] generateToken error: %v", user.Email, err)
		return nil, e.New(e.InvalidPassword)
	}
	data := resps.LoginResp{
		//UserInfo: user,
		Token: token,
	}

	return data, nil
}

func createLdapUser(c *ctx.ServiceContext, username, email string) (*models.User, e.Error) {
	// 登录成功, 在用户表中添加该用户
	user, err := services.CreateUser(c.DB(), models.User{
		Name:  username,
		Email: email,
	})
	if err != nil {
		c.Logger().Warnf("create user error: %v", err)
		return nil, e.New(e.InternalError, http.StatusInternalServerError)
	}

	return user, nil
}

func refreshLdapUserRole(c *ctx.ServiceContext, user *models.User, dn string) e.Error {
	tx := c.DB().Begin()
	defer func() {
		if r := recover(); r != nil {
			_ = tx.Rollback()
			panic(r)
		}
	}()

	// 获取ldap用户的OU信息
	userOU := strings.TrimPrefix(dn, fmt.Sprintf("uid=%s,", user.Name))
	c.Logger().Debugf("user ldap ou: %s", userOU)

	// 根据OU获取组织权限
	ldapUserOrgOUs, err := services.GetLdapOUOrgByDN(tx, userOU)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	// 更新用户组织权限
	c.Logger().Debugf("refresh ldap user org roles: %+v", ldapUserOrgOUs)
	err = services.RefreshUserOrgRoles(tx, user.Id, ldapUserOrgOUs)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	// 根据OU获取项目权限
	ldapUserProjectOUs, err := services.GetLdapOUProjectByDN(tx, userOU)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	// 更新用户项目权限
	c.Logger().Debugf("refresh ldap user project roles: %+v", ldapUserProjectOUs)
	err = services.RefreshUserProjectRoles(tx, user.Id, ldapUserProjectOUs)
	if err != nil {
		_ = tx.Rollback()
		return err
	}

	if err := tx.Commit(); err != nil {
		_ = tx.Rollback()
		c.Logger().Errorf("createLdapUserAndRole commit err: %s", err)
		return e.New(e.DBError, err)
	}

	return nil
}

// GenerateSsoToken 生成 SSO token
func GenerateSsoToken(c *ctx.ServiceContext) (resp interface{}, err e.Error) {

	token, er := services.GenerateSsoToken(c.UserId, 5*time.Minute)
	if er != nil {
		c.Logger().Errorf("userId [%s] generateToken error: %v", c.UserId, er)
		return nil, e.New(e.InternalError, er, http.StatusInternalServerError)
	}

	data := resps.SsoResp{
		Token: token,
	}

	return data, err
}

// VerifySsoToken 验证 SSO token
func VerifySsoToken(c *ctx.ServiceContext, form *forms.VerifySsoTokenForm) (resp *resps.VerifySsoTokenResp, err e.Error) {
	user, err := services.VerifySsoToken(c.DB(), form.Token)
	if err != nil {
		return nil, err
	}

	return &resps.VerifySsoTokenResp{
		UserId: user.Id,
		Email:  user.Email,
	}, nil
}
