package main

import (
	"context"
	"time"
)

func assemble(ctx context.Context, c *composition, life *lifecycle, builders constructors) error {
	parts, err := openStorage(ctx, c)
	for _, p := range parts {
		life.add(p)
	}
	if err != nil {
		return serverProblem("config", "database_file "+c.config.DatabaseFile+": cannot open storage", err)
	}
	if err = assembleOwners(ctx, c, life, builders); err != nil {
		return err
	}
	if err = newSchedules(c); err != nil {
		return err
	}
	// Replayed pushes may submit work, so wiring recovers before this phase.
	life.add(Part{Name: "pending pushes", Recover: c.replayPending})
	c.startupPassDone = make(chan struct{})
	life.add(Part{Name: "schedules", Run: c.startupSchedules})
	if c.auth == nil {
		c.auth = storeAuth{c.store}
	}
	if err = newTransport(ctx, c, c.diagnostic); err != nil {
		return err
	}
	c.health.sample = c.store.Readiness
	life.add(Part{Name: "health", Run: c.health.run, Ready: c.health.waitSample})
	life.add(Part{Name: "event retention", Run: func(ctx context.Context) error {
		return c.health.retainEvents(ctx, func(ctx context.Context, ordinary, progress time.Time) error {
			return c.store.RetainEvents(ctx, ordinary, progress)
		}, c.diagnostic)
	}})
	return nil
}

//nolint:contextcheck // Constructors retain the lifecycle context; ctx only bounds startup I/O.
func assembleOwners(ctx context.Context, c *composition, life *lifecycle, builders constructors) error {
	var err error
	if err = life.construct(c, builders.CacheNew); err != nil {
		return err
	}
	life.parts[len(life.parts)-1].Run = c.maintainCache
	if err = life.construct(c, builders.NativeNew); err != nil {
		return err
	}
	watch := watchPart(c)
	native := life.parts[len(life.parts)-1]
	if native.Run != nil {
		native.Run = nativeAfterWatch(watch.Ready, native.Run)
	}
	life.add(watch)
	if err = ctx.Err(); err != nil {
		return err
	}
	if err = life.construct(c, builders.WiringNew); err != nil {
		return err
	}
	if life.parts[len(life.parts)-1].Recover == nil {
		return missingPart("app.Recover")
	}
	for _, build := range []func(context.Context, *composition) (Part, error){builders.ServiceNew, builders.SourceNew} {
		if err = ctx.Err(); err != nil {
			return err
		}
		if err = life.construct(c, build); err != nil {
			return err
		}
	}
	return c.checkPorts()
}
