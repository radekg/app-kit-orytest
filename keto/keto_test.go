package keto

import (
	"testing"

	"github.com/radekg/app-kit-orytest/common"
	"github.com/radekg/app-kit-orytest/postgres"
	"github.com/stretchr/testify/assert"

	ketoRead "github.com/ory/keto-client-go/client/read"
	ketoWrite "github.com/ory/keto-client-go/client/write"
	ketoModels "github.com/ory/keto-client-go/models"
)

func TestKetoEmbedded(t *testing.T) {

	postgresCtx := postgres.SetupTestPostgres(t)
	ketoCtx := SetupTestKeto(t, postgresCtx, "default-namespace")
	defer ketoCtx.Cleanup()

	tuples := []*ketoModels.InternalRelationTuple{
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("company-a"),
			Relation:  common.StringP("employs"),
			Subject:   (*ketoModels.Subject)(common.StringP("director")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("company-a"),
			Relation:  common.StringP("employs"),
			Subject:   (*ketoModels.Subject)(common.StringP("it-staff")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("director"),
			Relation:  common.StringP("hires"),
			Subject:   (*ketoModels.Subject)(common.StringP("it-contractor")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("it-staff"),
			Relation:  common.StringP("subscribes"),
			Subject:   (*ketoModels.Subject)(common.StringP("dropbox")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("it-staff"),
			Relation:  common.StringP("subscribes"),
			Subject:   (*ketoModels.Subject)(common.StringP("aws")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("it-staff"),
			Relation:  common.StringP("subscribes"),
			Subject:   (*ketoModels.Subject)(common.StringP("gcp")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("company-a"),
			Relation:  common.StringP("pays"),
			Subject:   (*ketoModels.Subject)(common.StringP("default-namespace:company-a#employs")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("company-a"),
			Relation:  common.StringP("pays"),
			Subject:   (*ketoModels.Subject)(common.StringP("default-namespace:director#hires")),
		},
		{
			Namespace: common.StringP("default-namespace"),
			Object:    common.StringP("company-a"),
			Relation:  common.StringP("pays"),
			Subject:   (*ketoModels.Subject)(common.StringP("default-namespace:it-staff#subscribes")),
		},
	}

	for _, tuple := range tuples {
		_, err := ketoCtx.WriteClient().Write.CreateRelationTuple(ketoWrite.
			NewCreateRelationTupleParams().
			WithPayload(tuple))
		if err != nil {
			t.Fatal("Expected keto tuple to be created but received an error ", err)
		}
	}

	expandOK, expandErr := ketoCtx.ReadClient().Read.GetExpand(ketoRead.NewGetExpandParams().
		WithNamespace("default-namespace").
		WithObject("company-a").
		WithRelation("pays").WithMaxDepth(common.Int64P(100)))

	if expandErr != nil {
		t.Fatal("Expected keto expand to reply but received an error ", expandErr)
	}

	mapExpected := map[string][]string{
		"default-namespace:company-a#employs": {
			"director",
			"it-staff",
		},
		"default-namespace:director#hires": {
			"it-contractor",
		},
		"default-namespace:it-staff#subscribes": {
			"aws",
			"dropbox",
			"gcp",
		},
	}

	mapExpanded := map[string][]string{}

	for _, child := range expandOK.Payload.Children {
		if _, ok := mapExpanded[string(*child.Subject)]; !ok {
			mapExpanded[string(*child.Subject)] = []string{}
		}
		for _, innerChild := range child.Children {
			mapExpanded[string(*child.Subject)] = append(mapExpanded[string(*child.Subject)], string(*innerChild.Subject))
		}
	}

	assert.Equal(t, mapExpected, mapExpanded)
}
