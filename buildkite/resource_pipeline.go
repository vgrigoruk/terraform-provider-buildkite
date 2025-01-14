package buildkite

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/shurcooL/graphql"
)

// PipelineNode represents a pipeline as returned from the GraphQL API
type PipelineNode struct {
	CancelIntermediateBuilds             graphql.Boolean
	CancelIntermediateBuildsBranchFilter graphql.String
	DefaultBranch                        graphql.String
	Description                          graphql.String
	ID                                   graphql.String
	Name                                 graphql.String
	Repository                           struct {
		URL graphql.String
	}
	SkipIntermediateBuilds             graphql.Boolean
	SkipIntermediateBuildsBranchFilter graphql.String
	Slug                               graphql.String
	Steps                              struct {
		YAML graphql.String
	}
	Teams struct {
		Edges []struct {
			Node TeamPipelineNode
		}
	} `graphql:"teams(first: 50)"`
	WebhookURL graphql.String `graphql:"webhookURL"`
}

// PipelineAccessLevels represents a pipeline access levels as returned from the GraphQL API
type PipelineAccessLevels graphql.String

// TeamPipelineNode represents a team pipeline as returned from the GraphQL API
type TeamPipelineNode struct {
	AccessLevel PipelineAccessLevels
	ID          graphql.String
	Team        TeamNode
}

// resourcePipeline represents the terraform pipeline resource schema
func resourcePipeline() *schema.Resource {
	return &schema.Resource{
		CreateContext: CreatePipeline,
		ReadContext:   ReadPipeline,
		UpdateContext: UpdatePipeline,
		DeleteContext: DeletePipeline,
		Importer: &schema.ResourceImporter{
			State: schema.ImportStatePassthrough,
		},

		Schema: map[string]*schema.Schema{
			"cancel_intermediate_builds": {
				Optional: true,
				Default:  false,
				Type:     schema.TypeBool,
			},
			"cancel_intermediate_builds_branch_filter": {
				Optional: true,
				Default:  "",
				Type:     schema.TypeString,
			},
			"branch_configuration": {
				Type:     schema.TypeString,
				Optional: true,
			},
			"default_branch": {
				Optional: true,
				Type:     schema.TypeString,
			},
			"description": {
				Optional: true,
				Type:     schema.TypeString,
			},
			"name": {
				Required: true,
				Type:     schema.TypeString,
			},
			"repository": {
				Required: true,
				Type:     schema.TypeString,
			},
			"skip_intermediate_builds": {
				Optional: true,
				Default:  false,
				Type:     schema.TypeBool,
			},
			"skip_intermediate_builds_branch_filter": {
				Optional: true,
				Default:  "",
				Type:     schema.TypeString,
			},
			"slug": {
				Computed: true,
				Type:     schema.TypeString,
			},
			"steps": {
				Required: true,
				Type:     schema.TypeString,
			},
			"team": {
				Type:       schema.TypeSet,
				Optional:   true,
				ConfigMode: schema.SchemaConfigModeAttr,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"slug": {
							Required: true,
							Type:     schema.TypeString,
						},
						"access_level": {
							Required: true,
							Type:     schema.TypeString,
							ValidateFunc: func(val interface{}, key string) (warns []string, errs []error) {
								switch v := val.(string); v {
								case "READ_ONLY":
								case "BUILD_AND_READ":
								case "MANAGE_BUILD_AND_READ":
									return
								default:
									errs = append(errs, fmt.Errorf("%q must be one of READ_ONLY, BUILD_AND_READ or MANAGE_BUILD_AND_READ, got: %s", key, v))
									return
								}
								return
							},
						},
					},
				},
			},
			"webhook_url": {
				Computed: true,
				Type:     schema.TypeString,
			},
		},
	}
}

// CreatePipeline creates a Buildkite pipeline
func CreatePipeline(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*Client)
	orgID, err := GetOrganizationID(client.organization, client.graphql)
	if err != nil {
		return diag.FromErr(err)
	}

	var mutation struct {
		PipelineCreate struct {
			Pipeline PipelineNode
		} `graphql:"pipelineCreate(input: {cancelIntermediateBuilds: $cancel_intermediate_builds, cancelIntermediateBuildsBranchFilter: $cancel_intermediate_builds_branch_filter, defaultBranch: $default_branch, description: $desc, name: $name, organizationId: $org, repository: {url: $repository_url}, skipIntermediateBuilds: $skip_intermediate_builds, skipIntermediateBuildsBranchFilter: $skip_intermediate_builds_branch_filter, steps: {yaml: $steps}})"`
	}
	vars := map[string]interface{}{
		"cancel_intermediate_builds":               graphql.Boolean(d.Get("cancel_intermediate_builds").(bool)),
		"cancel_intermediate_builds_branch_filter": graphql.String(d.Get("cancel_intermediate_builds_branch_filter").(string)),
		"default_branch":                           graphql.String(d.Get("default_branch").(string)),
		"desc":                                     graphql.String(d.Get("description").(string)),
		"name":                                     graphql.String(d.Get("name").(string)),
		"org":                                      orgID,
		"repository_url":                           graphql.String(d.Get("repository").(string)),
		"skip_intermediate_builds":                 graphql.Boolean(d.Get("skip_intermediate_builds").(bool)),
		"skip_intermediate_builds_branch_filter":   graphql.String(d.Get("skip_intermediate_builds_branch_filter").(string)),
		"steps":                                    graphql.String(d.Get("steps").(string)),
	}

	log.Printf("Creating pipeline %s ...", vars["name"])
	err = client.graphql.Mutate(context.Background(), &mutation, vars)
	if err != nil {
		log.Printf("Unable to create pipeline %s", d.Get("name"))
		return diag.FromErr(err)
	}
	log.Printf("Successfully created pipeline with id '%s'.", mutation.PipelineCreate.Pipeline.ID)

	teamPipelines := getTeamPipelinesFromSchema(d)
	err = reconcileTeamPipelines(teamPipelines, &mutation.PipelineCreate.Pipeline, client)
	if err != nil {
		log.Print("Unable to create team pipelines")
		return diag.FromErr(err)
	}

	updatePipelineResource(d, &mutation.PipelineCreate.Pipeline)
	updatePipelineWithRESTfulAPI(d, client)

	return ReadPipeline(ctx, d, m)
}

// ReadPipeline retrieves a Buildkite pipeline
func ReadPipeline(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*Client)
	var query struct {
		Node struct {
			Pipeline PipelineNode `graphql:"... on Pipeline"`
		} `graphql:"node(id: $id)"`
	}
	vars := map[string]interface{}{
		"id": graphql.ID(d.Id()),
	}

	err := client.graphql.Query(context.Background(), &query, vars)
	if err != nil {
		return diag.FromErr(err)
	}

	updatePipelineResource(d, &query.Node.Pipeline)

	return nil
}

// UpdatePipeline updates a Buildkite pipeline
func UpdatePipeline(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*Client)
	var mutation struct {
		PipelineUpdate struct {
			Pipeline PipelineNode
		} `graphql:"pipelineUpdate(input: {cancelIntermediateBuilds: $cancel_intermediate_builds, cancelIntermediateBuildsBranchFilter: $cancel_intermediate_builds_branch_filter, defaultBranch: $default_branch, description: $desc, id: $id, name: $name, repository: {url: $repository_url}, skipIntermediateBuilds: $skip_intermediate_builds, skipIntermediateBuildsBranchFilter: $skip_intermediate_builds_branch_filter, steps: {yaml: $steps}})"`
	}
	vars := map[string]interface{}{
		"cancel_intermediate_builds":               graphql.Boolean(d.Get("cancel_intermediate_builds").(bool)),
		"cancel_intermediate_builds_branch_filter": graphql.String(d.Get("cancel_intermediate_builds_branch_filter").(string)),
		"default_branch":                           graphql.String(d.Get("default_branch").(string)),
		"desc":                                     graphql.String(d.Get("description").(string)),
		"id":                                       graphql.ID(d.Id()),
		"name":                                     graphql.String(d.Get("name").(string)),
		"repository_url":                           graphql.String(d.Get("repository").(string)),
		"skip_intermediate_builds":                 graphql.Boolean(d.Get("skip_intermediate_builds").(bool)),
		"skip_intermediate_builds_branch_filter":   graphql.String(d.Get("skip_intermediate_builds_branch_filter").(string)),
		"steps":                                    graphql.String(d.Get("steps").(string)),
	}

	log.Printf("Updating pipeline %s ...", vars["name"])
	err := client.graphql.Mutate(context.Background(), &mutation, vars)
	if err != nil {
		log.Printf("Unable to update pipeline %s", d.Get("name"))
		return diag.FromErr(err)
	}

	teamPipelines := getTeamPipelinesFromSchema(d)
	err = reconcileTeamPipelines(teamPipelines, &mutation.PipelineUpdate.Pipeline, client)
	if err != nil {
		log.Print("Unable to reconcile team pipelines")
		return diag.FromErr(err)
	}

	updatePipelineResource(d, &mutation.PipelineUpdate.Pipeline)
	updatePipelineWithRESTfulAPI(d, client)

	return ReadPipeline(ctx, d, m)
}

// DeletePipeline removes a Buildkite pipeline
func DeletePipeline(ctx context.Context, d *schema.ResourceData, m interface{}) diag.Diagnostics {
	client := m.(*Client)

	var mutation struct {
		PipelineDelete struct {
			Organization struct {
				ID graphql.ID
			}
		} `graphql:"pipelineDelete(input: {id: $id})"`
	}
	vars := map[string]interface{}{
		"id": graphql.ID(d.Id()),
	}

	log.Printf("Deleting pipeline %s ...", d.Get("name"))
	err := client.graphql.Mutate(context.Background(), &mutation, vars)
	if err != nil {
		log.Printf("Unable to delete pipeline %s", d.Get("name"))
		return diag.FromErr(err)
	}

	return nil
}

// As of August 7th 2020, GraphQL Pipeline is lacking support for updating properties:
// - branch_configuration
// - github provider configuration
// We fallback to REST API
func updatePipelineWithRESTfulAPI(d *schema.ResourceData, client *Client) error {
	slug := d.Get("slug").(string)
	log.Printf("Updating pipeline %s ...", slug)

	payload := map[string]interface{}{
		"branch_configuration": d.Get("branch_configuration").(string),
	}

	jsonStr, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	req, err := http.NewRequest("PATCH", fmt.Sprintf("https://api.buildkite.com/v2/organizations/%s/pipelines/%s",
		client.organization, slug), bytes.NewBuffer(jsonStr))
	if err != nil {
		return err
	}

	// a successful response returns 200
	resp, err := client.http.Do(req)
	if err != nil && resp.StatusCode != 200 {
		log.Printf("Unable to update pipeline %s", slug)
		return err
	}

	return nil
}

func getTeamPipelinesFromSchema(d *schema.ResourceData) []TeamPipelineNode {
	teamsInput := d.Get("team").(*schema.Set).List()
	teamPipelineNodes := make([]TeamPipelineNode, len(teamsInput))
	for i, v := range teamsInput {
		teamInput := v.(map[string]interface{})
		teamPipeline := TeamPipelineNode{
			AccessLevel: PipelineAccessLevels(teamInput["access_level"].(string)),
			ID:          "",
			Team: TeamNode{
				Slug: graphql.String(teamInput["slug"].(string)),
			},
		}
		teamPipelineNodes[i] = teamPipeline
	}
	return teamPipelineNodes
}

// reconcileTeamPipelines adds/updates/deletes the teamPipelines on buildkite to match the teams in terraform resource data
func reconcileTeamPipelines(teamPipelines []TeamPipelineNode, pipeline *PipelineNode, client *Client) error {
	teamPipelineIds := make(map[string]graphql.ID)

	var toAdd []TeamPipelineNode
	var toUpdate []TeamPipelineNode
	var toDelete []TeamPipelineNode

	// Look for teamPipelines on buildkite that need updated or removed
	for _, teamPipeline := range pipeline.Teams.Edges {
		teamSlugBk := teamPipeline.Node.Team.Slug
		accessLevelBk := teamPipeline.Node.AccessLevel
		id := teamPipeline.Node.ID

		teamPipelineIds[string(teamSlugBk)] = graphql.ID(id)

		found := false
		for _, teamPipeline := range teamPipelines {
			if teamPipeline.Team.Slug == teamSlugBk {
				found = true
				if teamPipeline.AccessLevel != accessLevelBk {
					toUpdate = append(toUpdate, TeamPipelineNode{
						AccessLevel: teamPipeline.AccessLevel,
						ID:          id,
						Team: TeamNode{
							Slug: teamPipeline.Team.Slug,
						},
					})
				}
			}
		}
		if !found {
			toDelete = append(toDelete, TeamPipelineNode{
				AccessLevel: accessLevelBk,
				ID:          id,
				Team: TeamNode{
					Slug: teamSlugBk,
				},
			})
		}
	}

	// Look for new teamsInput that need added to buildkite
	for _, teamPipeline := range teamPipelines {
		if _, found := teamPipelineIds[string(teamPipeline.Team.Slug)]; !found {
			toAdd = append(toAdd, teamPipeline)
		}
	}

	log.Printf("EXISTING_BUILDKITE_TEAMS: %s", teamPipelineIds)

	// Add any teamsInput that don't already exist
	err := createTeamPipelines(toAdd, string(pipeline.ID), client)
	if err != nil {
		return err
	}

	// Update any teamsInput access levels that need updating
	err = updateTeamPipelines(toUpdate, client)
	if err != nil {
		return err
	}

	// Remove any teamsInput that shouldn't exist
	err = deleteTeamPipelines(toDelete, client)
	if err != nil {
		return err
	}

	return nil
}

// createTeamPipelines grants access to a pipeline for the teams specified
func createTeamPipelines(teamPipelines []TeamPipelineNode, pipelineID string, client *Client) error {
	var mutation struct {
		TeamPipelineCreate struct {
			TeamPipeline struct {
				ID graphql.ID
			}
		} `graphql:"teamPipelineCreate(input: {teamID: $team, pipelineID: $pipeline, accessLevel: $accessLevel})"`
	}
	for _, teamPipeline := range teamPipelines {
		log.Printf("Granting teamPipeline %s %s access to pipeline id '%s'...", teamPipeline.Team.Slug, teamPipeline.AccessLevel, pipelineID)
		teamID, err := GetTeamID(string(teamPipeline.Team.Slug), client)
		if err != nil {
			log.Printf("Unable to get ID for team slug %s", teamPipeline.Team.Slug)
			return err
		}
		params := map[string]interface{}{
			"team":        graphql.ID(teamID),
			"pipeline":    graphql.ID(pipelineID),
			"accessLevel": teamPipeline.AccessLevel,
		}
		err = client.graphql.Mutate(context.Background(), &mutation, params)
		if err != nil {
			log.Printf("Unable to create team pipeline %s", teamPipeline.Team.Slug)
			return err
		}
	}
	return nil
}

// Update access levels for the given teamPipelines
func updateTeamPipelines(teamPipelines []TeamPipelineNode, client *Client) error {
	var mutation struct {
		TeamPipelineUpdate struct {
			TeamPipeline struct {
				ID graphql.ID
			}
		} `graphql:"teamPipelineUpdate(input: {id: $id, accessLevel: $accessLevel})"`
	}
	for _, teamPipeline := range teamPipelines {
		log.Printf("Updating access to %s for teamPipeline id '%s'...", teamPipeline.AccessLevel, teamPipeline.ID)
		params := map[string]interface{}{
			"id":          graphql.ID(string(teamPipeline.ID)),
			"accessLevel": teamPipeline.AccessLevel,
		}
		err := client.graphql.Mutate(context.Background(), &mutation, params)
		if err != nil {
			log.Printf("Unable to update team pipeline")
			return err
		}
	}
	return nil
}

func deleteTeamPipelines(teamPipelines []TeamPipelineNode, client *Client) error {
	var mutation struct {
		TeamPipelineDelete struct {
			Team struct {
				ID graphql.ID
			}
		} `graphql:"teamPipelineDelete(input: {id: $id})"`
	}
	for _, teamPipeline := range teamPipelines {
		log.Printf("Removing access for teamPipeline %s (id=%s)...", teamPipeline.Team.Slug, teamPipeline.ID)
		params := map[string]interface{}{
			"id": graphql.ID(string(teamPipeline.ID)),
		}
		err := client.graphql.Mutate(context.Background(), &mutation, params)
		if err != nil {
			log.Printf("Unable to delete team pipeline")
			return err
		}
	}

	return nil
}

// updatePipelineResource updates the terraform resource data for the pipeline resource
func updatePipelineResource(d *schema.ResourceData, pipeline *PipelineNode) {
	d.SetId(string(pipeline.ID))
	d.Set("cancel_intermediate_builds", bool(pipeline.CancelIntermediateBuilds))
	d.Set("cancel_intermediate_builds_branch_filter", string(pipeline.CancelIntermediateBuildsBranchFilter))
	d.Set("default_branch", string(pipeline.DefaultBranch))
	d.Set("description", string(pipeline.Description))
	d.Set("name", string(pipeline.Name))
	d.Set("repository", string(pipeline.Repository.URL))
	d.Set("skip_intermediate_builds", bool(pipeline.SkipIntermediateBuilds))
	d.Set("skip_intermediate_builds_branch_filter", string(pipeline.SkipIntermediateBuildsBranchFilter))
	d.Set("slug", string(pipeline.Slug))
	d.Set("steps", string(pipeline.Steps.YAML))
	d.Set("webhook_url", string(pipeline.WebhookURL))

	teams := make([]map[string]interface{}, len(pipeline.Teams.Edges))
	for i, id := range pipeline.Teams.Edges {
		team := map[string]interface{}{
			"slug":         string(id.Node.Team.Slug),
			"access_level": string(id.Node.AccessLevel),
		}
		teams[i] = team
	}
	d.Set("team", teams)
}
