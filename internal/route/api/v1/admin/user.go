// Copyright 2015 The Gogs Authors. All rights reserved.
// Use of this source code is governed by a MIT-style
// license that can be found in the LICENSE file.

package admin

import (
	"net/http"

	log "unknwon.dev/clog/v2"

	api "github.com/gogs/go-gogs-client"

	"github.com/G-Node/gogs/internal/conf"
	"github.com/G-Node/gogs/internal/context"
	"github.com/G-Node/gogs/internal/db"
	"github.com/G-Node/gogs/internal/db/errors"
	"github.com/G-Node/gogs/internal/mailer"
	"github.com/G-Node/gogs/internal/route/api/v1/user"
)

func parseLoginSource(c *context.APIContext, u *db.User, sourceID int64, loginName string) {
	if sourceID == 0 {
		return
	}

	source, err := db.GetLoginSourceByID(sourceID)
	if err != nil {
		if errors.IsLoginSourceNotExist(err) {
			c.Error(http.StatusUnprocessableEntity, "", err)
		} else {
			c.ServerError("GetLoginSourceByID", err)
		}
		return
	}

	u.LoginType = source.Type
	u.LoginSource = source.ID
	u.LoginName = loginName
}

func CreateUser(c *context.APIContext, form api.CreateUserOption) {
	u := &db.User{
		Name:      form.Username,
		FullName:  form.FullName,
		Email:     form.Email,
		Passwd:    form.Password,
		IsActive:  true,
		LoginType: db.LOGIN_PLAIN,
	}

	parseLoginSource(c, u, form.SourceID, form.LoginName)
	if c.Written() {
		return
	}

	if err := db.CreateUser(u); err != nil {
		if db.IsErrUserAlreadyExist(err) ||
			db.IsErrEmailAlreadyUsed(err) ||
			db.IsErrNameReserved(err) ||
			db.IsErrNamePatternNotAllowed(err) {
			c.Error(http.StatusUnprocessableEntity, "", err)
		} else {
			c.ServerError("CreateUser", err)
		}
		return
	}
	log.Trace("Account created by admin %q: %s", c.User.Name, u.Name)

	// Send email notification.
	if form.SendNotify && conf.MailService != nil {
		mailer.SendRegisterNotifyMail(c.Context.Context, db.NewMailerUser(u))
	}

	c.JSON(http.StatusCreated, u.APIFormat())
}

func EditUser(c *context.APIContext, form api.EditUserOption) {
	u := user.GetUserByParams(c)
	if c.Written() {
		return
	}

	parseLoginSource(c, u, form.SourceID, form.LoginName)
	if c.Written() {
		return
	}

	if len(form.Password) > 0 {
		u.Passwd = form.Password
		var err error
		if u.Salt, err = db.GetUserSalt(); err != nil {
			c.ServerError("GetUserSalt", err)
			return
		}
		u.EncodePasswd()
	}

	u.LoginName = form.LoginName
	u.FullName = form.FullName
	u.Email = form.Email
	u.Website = form.Website
	u.Location = form.Location
	if form.Active != nil {
		u.IsActive = *form.Active
	}
	if form.Admin != nil {
		u.IsAdmin = *form.Admin
	}
	if form.AllowGitHook != nil {
		u.AllowGitHook = *form.AllowGitHook
	}
	if form.AllowImportLocal != nil {
		u.AllowImportLocal = *form.AllowImportLocal
	}
	if form.MaxRepoCreation != nil {
		u.MaxRepoCreation = *form.MaxRepoCreation
	}

	if err := db.UpdateUser(u); err != nil {
		if db.IsErrEmailAlreadyUsed(err) {
			c.Error(http.StatusUnprocessableEntity, "", err)
		} else {
			c.ServerError("UpdateUser", err)
		}
		return
	}
	log.Trace("Account profile updated by admin %q: %s", c.User.Name, u.Name)

	c.JSONSuccess(u.APIFormat())
}

func DeleteUser(c *context.APIContext) {
	u := user.GetUserByParams(c)
	if c.Written() {
		return
	}

	if err := db.DeleteUser(u); err != nil {
		if db.IsErrUserOwnRepos(err) ||
			db.IsErrUserHasOrgs(err) {
			c.Error(http.StatusUnprocessableEntity, "", err)
		} else {
			c.ServerError("DeleteUser", err)
		}
		return
	}
	log.Trace("Account deleted by admin(%s): %s", c.User.Name, u.Name)

	c.NoContent()
}

func CreatePublicKey(c *context.APIContext, form api.CreateKeyOption) {
	u := user.GetUserByParams(c)
	if c.Written() {
		return
	}
	user.CreateUserPublicKey(c, form, u.ID)
}
