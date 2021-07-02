package forms

import "cloudiac/portal/models"

type CreateTokenForm struct {
	PageForm

	Description string `form:"description" json:"description" binding:""`
}

type UpdateTokenForm struct {
	PageForm
	Id          models.Id `form:"id" json:"id" binding:"required"`
	Status      string    `form:"status" json:"status" binding:"required"`
	Description string    `form:"description" json:"description" binding:""`
}

type SearchTokenForm struct {
	PageForm
	Q      string `form:"q" json:"q" binding:""`
	Status string `form:"status" json:"status"`
}

type DeleteTokenForm struct {
	PageForm
	Id models.Id `form:"id" json:"id" binding:"required"`
}
