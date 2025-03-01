// Copyright (c) 2015-2023 CloudJ Technology Co., Ltd.

package resps

import "cloudiac/portal/models"

type OrgDetailResp struct {
	models.Organization
	Creator string `json:"creator" example:"研发部负责人"` // 创建人名称
}

type OrganizationDetailResp struct {
	models.Organization
	Creator string `json:"creator" example:"超级管理员"`
}

type OrgOrProjectResourcesResp struct {
	ProjectName  string    `json:"projectName"`
	EnvName      string    `json:"envName"`
	ResourceName string    `json:"resourceName"`
	Provider     string    `json:"provider"`
	Type         string    `json:"type"`
	Module       string    `json:"module"`
	EnvId        models.Id `json:"envId"`
	ProjectId    models.Id `json:"projectId"`
	ResourceId   models.Id `json:"resourceId"`
	Attrs        string    `json:"attrs"`
	Dependencies string    `json:"dependencies"`
}

type InviteUsersBatchResp struct {
	Success int `json:"success"`
	Failed  int `json:"failed"`
}

type EnvResp struct {
	EnvName string    `json:"envName"`
	EnvId   models.Id `json:"envId"`
}

type OrgProjectResp struct {
	ProjectName string    `json:"projectName"`
	ProjectId   models.Id `json:"projectId"`
}

type OrgEnvAndProviderResp struct {
	Envs      []EnvResp `json:"envs"`
	Providers []string  `json:"providers"`
}

type OrgProjectAndProviderResp struct {
	Projects  []OrgProjectResp `json:"projects"`
	Providers []string         `json:"providers"`
}

type DetailStatResp struct {
	Id    models.Id `json:"id"`
	Name  string    `json:"name"`
	Count int       `json:"count"`
}

type EnvStatResp struct {
	Status  string           `json:"status"`
	Count   int              `json:"count"`
	Details []DetailStatResp `json:"details"`
}

type ResStatResp struct {
	ResType string           `json:"resType"`
	Count   int              `json:"count"`
	Details []DetailStatResp `json:"details"`
}

type DetailStatWithUpResp struct {
	Id    models.Id `json:"id"`
	Name  string    `json:"name"`
	Count int       `json:"count"`
	Up    int       `json:"up"`
}

type ResTypeDetailStatWithUpResp struct {
	ResType string                 `json:"resType"`
	Count   int                    `json:"count"`
	Up      int                    `json:"up"`
	Details []DetailStatWithUpResp `json:"details"`
}

type ProjOrEnvResStatResp struct {
	ResType string           `json:"resType"`
	Count   int              `json:"count"`
	Details []DetailStatResp `json:"details"`
}

type ResGrowTrendResp struct {
	Date    string           `json:"date"`
	Count   int              `json:"count"`
	Details []DetailStatResp `json:"details"`
}

type OrgProjectsStatResp struct {
	EnvStat        []EnvStatResp          `json:"envStat"`
	ResStat        []ResStatResp          `json:"resStat"`
	ProjectResStat []ProjOrEnvResStatResp `json:"projectResStat"`
	ResGrowTrend   []ResGrowTrendResp     `json:"resGrowTrend"`
}
