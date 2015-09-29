package main

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/prometheus/common/model"
	"golang.org/x/net/context"

	"github.com/prometheus/alertmanager/config"
	"github.com/prometheus/alertmanager/provider"
	"github.com/prometheus/alertmanager/types"
)

type recordNotifier struct {
	ctx    context.Context
	alerts []*types.Alert
}

func (n *recordNotifier) Notify(ctx context.Context, as ...*types.Alert) error {
	n.ctx = ctx
	n.alerts = append(n.alerts, as...)
	return nil
}

type failNotifier struct{}

func (n *failNotifier) Notify(ctx context.Context, as ...*types.Alert) error {
	return fmt.Errorf("")
}

func TestDedupingNotifier(t *testing.T) {
	var (
		record   = &recordNotifier{}
		notifies = provider.NewMemNotifies(provider.NewMemData())
		deduper  = newDedupingNotifier(notifies, record)
		ctx      = context.Background()
	)
	ctx = context.WithValue(ctx, notifyName, "name")
	ctx = context.WithValue(ctx, notifyRepeatInterval, time.Duration(100*time.Minute))
	ctx = context.WithValue(ctx, notifySendResolved, true)

	now := time.Now()

	alerts := []*types.Alert{
		{
			Labels: model.LabelSet{"alertname": "1"},
		},
		{
			Labels: model.LabelSet{"alertname": "2"},
		},
		{
			Labels:     model.LabelSet{"alertname": "3"},
			ResolvedAt: now.Add(-20 * time.Minute),
		},
		{
			Labels:     model.LabelSet{"alertname": "4"},
			ResolvedAt: now.Add(-10 * time.Minute),
		},
		{
			Labels:     model.LabelSet{"alertname": "5"},
			ResolvedAt: now.Add(-10 * time.Minute),
		},
		{
			Labels: model.LabelSet{"alertname": "6"},
		},
	}

	var fps []model.Fingerprint
	for _, a := range alerts {
		fps = append(fps, a.Fingerprint())
	}

	nsBefore := []*types.Notify{
		// The first alert is another attempt to send a previously
		// failing and firing notification.
		{
			Alert:     fps[0],
			SendTo:    "name",
			Resolved:  false,
			Delivered: false,
			Timestamp: now.Add(-20 * time.Minute),
		},
		// The second alert comes through for the first time and
		// is omitted here.
		nil,
		// The third alert is another attempt to send a previously
		// failing and resolved notification.
		{
			Alert:     fps[2],
			SendTo:    "name",
			Resolved:  true,
			Delivered: false,
			Timestamp: now.Add(-10 * time.Minute),
		},
		// The fourth alert is an attempt to resolve a previously
		// firing and delivered alert.
		{
			Alert:     fps[3],
			SendTo:    "name",
			Resolved:  false,
			Delivered: true,
			Timestamp: now.Add(-10 * time.Minute),
		},
		// The fifth alert is an attempt to resolve an alert again
		// even though the previous notification succeeded.
		{
			Alert:     fps[4],
			SendTo:    "name",
			Resolved:  true,
			Delivered: true,
			Timestamp: now.Add(-10 * time.Minute),
		},
		// The sixth alert resends a previously successful notification
		// that was longer than ago than the repeat interval.
		{
			Alert:     fps[5],
			SendTo:    "name",
			Resolved:  false,
			Delivered: true,
			Timestamp: now.Add(-110 * time.Minute),
		},
	}

	if err := notifies.Set("name", nsBefore...); err != nil {
		t.Fatalf("Setting notifies failed: %s", err)
	}

	deduper.notifier = &failNotifier{}
	if err := deduper.Notify(ctx, alerts...); err == nil {
		t.Fatalf("Fail notifier did not fail")
	}
	// After a failing notify the notifies data must be unchanged.
	nsCur, err := notifies.Get("name", fps...)
	if err != nil {
		t.Fatalf("Error getting notifies", err)
	}
	if !reflect.DeepEqual(nsBefore, nsCur) {
		t.Fatalf("Notifies data has changed unexpectedly")
	}

	deduper.notifier = record
	if err := deduper.Notify(ctx, alerts...); err != nil {
		t.Fatalf("Notify failed: %s", err)
	}

	alertsExp := []*types.Alert{
		alerts[0],
		alerts[1],
		alerts[2],
		alerts[3],
		alerts[5],
	}

	nsAfter := []*types.Notify{
		{
			Alert:     fps[0],
			SendTo:    "name",
			Resolved:  false,
			Delivered: true,
		},
		{
			Alert:     fps[1],
			SendTo:    "name",
			Resolved:  false,
			Delivered: true,
		},
		{
			Alert:     fps[2],
			SendTo:    "name",
			Resolved:  true,
			Delivered: true,
		},
		{
			Alert:     fps[3],
			SendTo:    "name",
			Resolved:  true,
			Delivered: true,
		},
		// Unmodified.
		{
			Alert:     fps[4],
			SendTo:    "name",
			Resolved:  true,
			Delivered: true,
			Timestamp: now.Add(-10 * time.Minute),
		},
		{
			Alert:     fps[5],
			SendTo:    "name",
			Resolved:  false,
			Delivered: true,
		},
	}

	if !reflect.DeepEqual(record.alerts, alertsExp) {
		t.Fatalf("Expected alerts %v, got %v", alertsExp, record.alerts)
	}
	nsCur, err = notifies.Get("name", fps...)
	if err != nil {
		t.Fatalf("Error getting notifies", err)
	}

	// Hack correct timestamps back in if they are sane.
	for i, after := range nsAfter {
		cur := nsCur[i]
		if after.Timestamp.IsZero() {
			if cur.Timestamp.Before(now) {
				t.Fatalf("Wrong timestamp for notify %v", cur)
			}
			after.Timestamp = cur.Timestamp
		}
		if !reflect.DeepEqual(after, cur) {
			t.Errorf("Unexpected notifies, expected: %v, got: %v", after, cur)
		}
	}
}

func TestRoutedNotifier(t *testing.T) {
	notifiers := map[string]Notifier{
		"1": &recordNotifier{},
		"2": &recordNotifier{},
		"3": &recordNotifier{},
	}
	notifierOpts := map[string]*config.NotificationConfig{
		"1": &config.NotificationConfig{
			SendResolved:   false,
			RepeatInterval: 10000,
		},
		"2": &config.NotificationConfig{
			SendResolved:   true,
			RepeatInterval: 20000,
		},
		"3": &config.NotificationConfig{
			SendResolved:   false,
			RepeatInterval: 30000,
		},
	}
	routed := &routedNotifier{
		notifiers:    notifiers,
		notifierOpts: notifierOpts,
	}

	for _, route := range []string{"3", "2", "1"} {
		var (
			ctx   = context.WithValue(context.Background(), notifyName, route)
			alert = &types.Alert{
				Labels: model.LabelSet{"route": model.LabelValue(route)},
			}
		)
		err := routed.Notify(ctx, alert)
		if err != nil {
			t.Fatal(err)
		}

		rn := routed.notifiers[route].(*recordNotifier)
		if len(rn.alerts) != 1 && alert != rn.alerts[0] {
			t.Fatalf("Expeceted alert %v, got %v", alert, rn.alerts)
		}

		// The context handed down the chain must be populated with the
		// necessary information of the notification config.
		name, ok := rn.ctx.Value(notifyName).(string)
		if !ok || name != route {
			t.Fatalf("Expected name %q, got %q", name, route)
		}

		repeatInterval, ok := rn.ctx.Value(notifyRepeatInterval).(time.Duration)
		if ri := notifierOpts[route].RepeatInterval; !ok || repeatInterval != time.Duration(ri) {
			t.Fatalf("Expected repeat interval %q, got %q", ri, repeatInterval)
		}

		sendResolved, ok := rn.ctx.Value(notifySendResolved).(bool)
		if sr := notifierOpts[route].SendResolved; !ok || sendResolved != sr {
			t.Fatalf("Expected send resolved %q, got %q", sr, sendResolved)
		}
	}
}

func TestMutingNotifier(t *testing.T) {
	// Mute all label sets that have a "mute" key.
	muter := types.MuteFunc(func(lset model.LabelSet) bool {
		_, ok := lset["mute"]
		return ok
	})

	record := &recordNotifier{}
	muteNotifer := mutingNotifier{
		notifier: record,
		Muter:    muter,
	}

	in := []model.LabelSet{
		{},
		{"test": "set"},
		{"mute": "me"},
		{"foo": "bar", "test": "set"},
		{"foo": "bar", "mute": "me"},
		{},
		{"not": "muted"},
	}
	out := []model.LabelSet{
		{},
		{"test": "set"},
		{"foo": "bar", "test": "set"},
		{},
		{"not": "muted"},
	}

	var inAlerts []*types.Alert
	for _, lset := range in {
		inAlerts = append(inAlerts, &types.Alert{Labels: lset})
	}

	if err := muteNotifer.Notify(nil, inAlerts...); err != nil {
		t.Fatalf("Notifying failed: %s", err)
	}

	var got []model.LabelSet
	for _, a := range record.alerts {
		got = append(got, a.Labels)
	}

	if !reflect.DeepEqual(got, out) {
		t.Fatalf("Muting failed, expected: %v\ngot %v", out, got)
	}
}