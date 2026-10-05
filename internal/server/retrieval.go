package server

import (
	"fmt"
	"net/url"
)

func aliasedInt(q url.Values, name, alias string, fallback int) (int, error) {
	value, err := nonNegativeInt(q, name, fallback)
	if err != nil {
		return 0, err
	}
	other, err := nonNegativeInt(q, alias, fallback)
	if err != nil {
		return 0, err
	}
	if q.Get(name) != "" && q.Get(alias) != "" && value != other {
		return 0, fmt.Errorf("%s and %s disagree", name, alias)
	}
	if q.Get(alias) != "" {
		return other, nil
	}
	return value, nil
}

type retrievalOptions struct {
	hours, windowMessages, perSession int
	reduceNoise                       bool
}

func parseRetrieval(q url.Values) (retrievalOptions, error) {
	var opts retrievalOptions
	var err error
	opts.hours, err = aliasedInt(q, "window_hours", "hours", 0)
	if err != nil {
		return opts, err
	}
	opts.windowMessages, err = nonNegativeInt(q, "window_messages", 0)
	if err != nil {
		return opts, err
	}
	opts.perSession, err = nonNegativeInt(q, "per_session", 0)
	if err != nil {
		return opts, err
	}
	opts.reduceNoise, err = boolParam(q, "reduce_noise")
	return opts, err
}
