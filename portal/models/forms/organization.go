package forms

import "cloudiac/portal/models"

type CreateOrganizationForm struct {
	BaseForm
	Name        string `form:"name" json:"name" binding:"required,gte=2,lte=32"` // 组织名称
	Description string `form:"description" json:"description" binding:""`        // 组织描述
	RunnerId    string `form:"runnerId" json:"runnerId" binding:""`              // 组织默认部署通道
}

type UpdateOrganizationForm struct {
	BaseForm

	Id models.Id `uri:"id" json:"id" swaggerignore:"true"` // 组织ID，swagger 参数通过 param path 指定，这里忽略

	Name        string `form:"name" json:"name" binding:""`                      // 组织名称
	Description string `form:"description" json:"description" binding:"max=255"` // 组织描述
	RunnerId    string `form:"runnerId" json:"runnerId" binding:""`              // 组织默认部署通道
	Status      string `form:"status" json:"status" enums:"enable,disable"`      // 组织状态
}

type SearchOrganizationForm struct {
	PageForm

	Q      string `form:"q" json:"q" binding:""`                       // 组织名称，支持模糊查询
	Status string `form:"status" json:"status" enums:"enable,disable"` // 组织状态
}

type DeleteOrganizationForm struct {
	BaseForm

	Id models.Id `uri:"id" json:"id" swaggerignore:"true"` // 组织ID，swagger 参数通过 param path 指定，这里忽略
}

type DisableOrganizationForm struct {
	BaseForm

	Id models.Id `uri:"id" json:"id" swaggerignore:"true"` // 组织ID，swagger 参数通过 param path 指定，这里忽略

	Status string `form:"status" json:"status" binding:"required" enums:"enable,disable"` // 组织状态
}

type DetailOrganizationForm struct {
	BaseForm

	Id models.Id `uri:"id" json:"id" swaggerignore:"true"` // 组织ID，swagger 参数通过 param path 指定，这里忽略
}

type OrganizationParam struct {
	BaseForm

	Id models.Id `uri:"id" json:"id" swaggerignore:"true"` // 组织ID
}
