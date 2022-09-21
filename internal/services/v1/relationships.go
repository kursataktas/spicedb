package v1

import (
	"context"
	"errors"

	"github.com/authzed/spicedb/internal/util"

	v1 "github.com/authzed/authzed-go/proto/authzed/api/v1"
	grpcmw "github.com/grpc-ecosystem/go-grpc-middleware"
	grpcvalidate "github.com/grpc-ecosystem/go-grpc-middleware/validator"
	"github.com/jzelinskie/stringz"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/authzed/spicedb/internal/dispatch"
	datastoremw "github.com/authzed/spicedb/internal/middleware/datastore"
	"github.com/authzed/spicedb/internal/middleware/handwrittenvalidation"
	"github.com/authzed/spicedb/internal/middleware/usagemetrics"
	"github.com/authzed/spicedb/internal/namespace"
	"github.com/authzed/spicedb/internal/services/shared"
	"github.com/authzed/spicedb/pkg/datastore"
	"github.com/authzed/spicedb/pkg/middleware/consistency"
	dispatchv1 "github.com/authzed/spicedb/pkg/proto/dispatch/v1"
	"github.com/authzed/spicedb/pkg/tuple"
	"github.com/authzed/spicedb/pkg/zedtoken"
)

// PermissionsServerConfig is configuration for the permissions server.
type PermissionsServerConfig struct {
	// MaxUpdatesPerWrite holds the maximum number of updates allowed per
	// WriteRelationships call.
	MaxUpdatesPerWrite uint16

	// MaxPreconditionsCount holds the maximum number of preconditions allowed
	// on a WriteRelationships or DeleteRelationships call.
	MaxPreconditionsCount uint16

	// MaximumAPIDepth is the default/starting depth remaining for API calls made
	// to the permissions server.
	MaximumAPIDepth uint32
}

// NewPermissionsServer creates a PermissionsServiceServer instance.
func NewPermissionsServer(
	dispatch dispatch.Dispatcher,
	config PermissionsServerConfig,
) v1.PermissionsServiceServer {
	configWithDefaults := PermissionsServerConfig{
		MaxPreconditionsCount: defaultIfZero(config.MaxPreconditionsCount, 1000),
		MaxUpdatesPerWrite:    defaultIfZero(config.MaxUpdatesPerWrite, 1000),
		MaximumAPIDepth:       defaultIfZero(config.MaximumAPIDepth, 50),
	}

	return &permissionServer{
		dispatch: dispatch,
		config:   configWithDefaults,
		WithServiceSpecificInterceptors: shared.WithServiceSpecificInterceptors{
			Unary: grpcmw.ChainUnaryServer(
				grpcvalidate.UnaryServerInterceptor(),
				handwrittenvalidation.UnaryServerInterceptor,
				usagemetrics.UnaryServerInterceptor(),
			),
			Stream: grpcmw.ChainStreamServer(
				grpcvalidate.StreamServerInterceptor(),
				handwrittenvalidation.StreamServerInterceptor,
				usagemetrics.StreamServerInterceptor(),
			),
		},
	}
}

type permissionServer struct {
	v1.UnimplementedPermissionsServiceServer
	shared.WithServiceSpecificInterceptors

	dispatch dispatch.Dispatcher
	config   PermissionsServerConfig
}

func (ps *permissionServer) checkFilterComponent(ctx context.Context, objectType, optionalRelation string, ds datastore.Reader) error {
	relationToTest := stringz.DefaultEmpty(optionalRelation, datastore.Ellipsis)
	allowEllipsis := optionalRelation == ""
	return namespace.CheckNamespaceAndRelation(ctx, objectType, relationToTest, allowEllipsis, ds)
}

func (ps *permissionServer) checkFilterNamespaces(ctx context.Context, filter *v1.RelationshipFilter, ds datastore.Reader) error {
	if err := ps.checkFilterComponent(ctx, filter.ResourceType, filter.OptionalRelation, ds); err != nil {
		return err
	}

	if subjectFilter := filter.OptionalSubjectFilter; subjectFilter != nil {
		subjectRelation := ""
		if subjectFilter.OptionalRelation != nil {
			subjectRelation = subjectFilter.OptionalRelation.Relation
		}
		if err := ps.checkFilterComponent(ctx, subjectFilter.SubjectType, subjectRelation, ds); err != nil {
			return err
		}
	}

	return nil
}

func (ps *permissionServer) ReadRelationships(req *v1.ReadRelationshipsRequest, resp v1.PermissionsService_ReadRelationshipsServer) error {
	ctx := resp.Context()
	atRevision, revisionReadAt := consistency.MustRevisionFromContext(ctx)
	ds := datastoremw.MustFromContext(ctx).SnapshotReader(atRevision)

	if err := ps.checkFilterNamespaces(ctx, req.RelationshipFilter, ds); err != nil {
		return rewriteError(ctx, err)
	}

	usagemetrics.SetInContext(ctx, &dispatchv1.ResponseMeta{
		DispatchCount: 1,
	})

	tupleIterator, err := ds.QueryRelationships(ctx, datastore.RelationshipsFilterFromPublicFilter(req.RelationshipFilter))
	if err != nil {
		return rewriteError(ctx, err)
	}
	defer tupleIterator.Close()

	for tpl := tupleIterator.Next(); tpl != nil; tpl = tupleIterator.Next() {
		err := resp.Send(&v1.ReadRelationshipsResponse{
			ReadAt:       revisionReadAt,
			Relationship: tuple.ToRelationship(tpl),
		})
		if err != nil {
			return err
		}
	}
	if tupleIterator.Err() != nil {
		return status.Errorf(codes.Internal, "error when reading tuples: %s", tupleIterator.Err())
	}

	return nil
}

func (ps *permissionServer) WriteRelationships(ctx context.Context, req *v1.WriteRelationshipsRequest) (*v1.WriteRelationshipsResponse, error) {
	ds := datastoremw.MustFromContext(ctx)

	// Ensure that the updates and preconditions are not over the configured limits.
	if len(req.Updates) > int(ps.config.MaxUpdatesPerWrite) {
		return nil, rewriteError(
			ctx,
			NewExceedsMaximumUpdatesErr(uint16(len(req.Updates)), ps.config.MaxUpdatesPerWrite),
		)
	}

	if len(req.OptionalPreconditions) > int(ps.config.MaxPreconditionsCount) {
		return nil, rewriteError(
			ctx,
			NewExceedsMaximumPreconditionsErr(uint16(len(req.OptionalPreconditions)), ps.config.MaxPreconditionsCount),
		)
	}

	// Check for duplicate updates.
	updateRelationshipSet := util.NewSet[string]()
	for _, update := range req.Updates {
		tupleStr := tuple.StringRelationship(update.Relationship)
		if !updateRelationshipSet.Add(tupleStr) {
			return nil, rewriteError(
				ctx,
				status.Errorf(
					codes.InvalidArgument,
					"found duplicate update operation for relationship %s",
					tupleStr,
				),
			)
		}
	}

	// Execute the write operation(s).
	revision, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		for _, precond := range req.OptionalPreconditions {
			if err := ps.checkFilterNamespaces(ctx, precond.Filter, rwt); err != nil {
				return err
			}
		}
		for _, update := range req.Updates {
			// TODO(vroldanbet) we type assert CaveatReader for now because not all datastores implement it
			// eventually this type assertion should be removed
			if update.Relationship.OptionalCaveat != nil && update.Relationship.OptionalCaveat.CaveatName != "" {
				if caveatReader, ok := rwt.(datastore.CaveatReader); ok {
					_, err := caveatReader.ReadCaveatByName(update.Relationship.OptionalCaveat.CaveatName)
					if errors.As(err, &datastore.ErrCaveatNameNotFound{}) {
						return rewriteError(ctx, NewCaveatNotFoundError(update))
					}

					if err != nil {
						return rewriteError(ctx, err)
					}
				} else {
					return status.Errorf(
						codes.FailedPrecondition,
						"caveats are currently not supported for this datastore; found for relationship `%s`",
						tuple.StringRelationship(update.Relationship),
					)
				}
			}

			if err := tuple.ValidateResourceID(update.Relationship.Resource.ObjectId); err != nil {
				return err
			}

			if err := tuple.ValidateSubjectID(update.Relationship.Subject.Object.ObjectId); err != nil {
				return err
			}

			if err := namespace.CheckNamespaceAndRelation(
				ctx,
				update.Relationship.Resource.ObjectType,
				update.Relationship.Relation,
				false,
				rwt,
			); err != nil {
				return err
			}

			if err := namespace.CheckNamespaceAndRelation(
				ctx,
				update.Relationship.Subject.Object.ObjectType,
				stringz.DefaultEmpty(update.Relationship.Subject.OptionalRelation, datastore.Ellipsis),
				true,
				rwt,
			); err != nil {
				return err
			}

			_, ts, err := namespace.ReadNamespaceAndTypes(
				ctx,
				update.Relationship.Resource.ObjectType,
				rwt,
			)
			if err != nil {
				return err
			}

			if ts.IsPermission(update.Relationship.Relation) {
				return status.Errorf(
					codes.InvalidArgument,
					"cannot write a relationship to permission %s",
					update.Relationship.Relation,
				)
			}

			if update.Relationship.Subject.Object.ObjectId == tuple.PublicWildcard {
				isAllowed, err := ts.IsAllowedPublicNamespace(
					update.Relationship.Relation,
					update.Relationship.Subject.Object.ObjectType)
				if err != nil {
					return err
				}

				if isAllowed != namespace.PublicSubjectAllowed {
					return status.Errorf(
						codes.InvalidArgument,
						"wildcard subjects of type %s are not allowed on %v",
						update.Relationship.Subject.Object.ObjectType,
						tuple.StringObjectRef(update.Relationship.Resource),
					)
				}
			} else {
				isAllowed, err := ts.IsAllowedDirectRelation(
					update.Relationship.Relation,
					update.Relationship.Subject.Object.ObjectType,
					stringz.DefaultEmpty(update.Relationship.Subject.OptionalRelation, datastore.Ellipsis),
				)
				if err != nil {
					return err
				}

				if isAllowed == namespace.DirectRelationNotValid {
					return status.Errorf(
						codes.InvalidArgument,
						"subject %s is not allowed for the resource %s",
						tuple.StringSubjectRef(update.Relationship.Subject),
						tuple.StringObjectRef(update.Relationship.Resource),
					)
				}
			}
		}

		usagemetrics.SetInContext(ctx, &dispatchv1.ResponseMeta{
			// One request per precondition and one request for the actual writes.
			DispatchCount: uint32(len(req.OptionalPreconditions)) + 1,
		})

		if err := checkPreconditions(ctx, rwt, req.OptionalPreconditions); err != nil {
			return err
		}

		return rwt.WriteRelationships(tuple.UpdateFromRelationshipUpdates(req.Updates))
	})
	if err != nil {
		return nil, rewriteError(ctx, err)
	}

	return &v1.WriteRelationshipsResponse{
		WrittenAt: zedtoken.NewFromRevision(revision),
	}, nil
}

func (ps *permissionServer) DeleteRelationships(ctx context.Context, req *v1.DeleteRelationshipsRequest) (*v1.DeleteRelationshipsResponse, error) {
	if len(req.OptionalPreconditions) > int(ps.config.MaxPreconditionsCount) {
		return nil, rewriteError(
			ctx,
			NewExceedsMaximumPreconditionsErr(uint16(len(req.OptionalPreconditions)), ps.config.MaxPreconditionsCount),
		)
	}

	ds := datastoremw.MustFromContext(ctx)

	revision, err := ds.ReadWriteTx(ctx, func(ctx context.Context, rwt datastore.ReadWriteTransaction) error {
		if err := ps.checkFilterNamespaces(ctx, req.RelationshipFilter, rwt); err != nil {
			return err
		}

		usagemetrics.SetInContext(ctx, &dispatchv1.ResponseMeta{
			// One request per precondition and one request for the actual delete.
			DispatchCount: uint32(len(req.OptionalPreconditions)) + 1,
		})

		if err := checkPreconditions(ctx, rwt, req.OptionalPreconditions); err != nil {
			return err
		}

		return rwt.DeleteRelationships(req.RelationshipFilter)
	})
	if err != nil {
		return nil, rewriteError(ctx, err)
	}

	return &v1.DeleteRelationshipsResponse{
		DeletedAt: zedtoken.NewFromRevision(revision),
	}, nil
}
