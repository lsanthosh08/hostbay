package netd

// ProxyState is a network service's automatic proxy setting: the PAC URL
// and whether it is on.
type ProxyState struct {
	URL string `json:"url,omitempty"`
	On  bool   `json:"on,omitempty"`
}

// Proxies is the system's per-service automatic proxy settings. macOS has
// one (proxy_darwin.go); elsewhere the helper has none and answers proxy
// requests CodeUnsupported. Tests use a fake.
type Proxies interface {
	// Services lists the enabled network services.
	Services() ([]string, error)
	Get(service string) (ProxyState, error)
	// Set applies st; an empty URL only switches the setting off.
	Set(service string, st ProxyState) error
}

// proxy points every enabled service at m.URL, journaling each service's
// setting before it first changes, or with ClearProxy puts them all back.
func (s *Server) proxy(m Msg) *Error {
	if s.Proxies == nil {
		return &Error{CodeUnsupported, "This helper cannot set the system proxy."}
	}
	if err := validate(m); err != nil {
		return err
	}
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if s.stopped {
		return refused("The helper is stopping.")
	}
	if m.T == ClearProxy {
		if err := s.Journal.RestoreProxies(s.Proxies); err != nil {
			return failed("%v", err)
		}
		return nil
	}
	svcs, err := s.Proxies.Services()
	if err != nil {
		return failed("listing network services: %v", err)
	}
	for _, svc := range svcs {
		if !s.Journal.proxied(svc) {
			prior, err := s.Proxies.Get(svc)
			if err != nil {
				return failed("reading the proxy of %s: %v", svc, err)
			}
			if ownPAC(prior.URL) {
				prior = ProxyState{} // left by a run whose sweep never came: it was off before that
			}
			if err := s.Journal.recordProxy(svc, prior); err != nil {
				return failed("journaling the proxy of %s: %v", svc, err)
			}
		}
		if err := s.Proxies.Set(svc, ProxyState{URL: m.URL, On: true}); err != nil {
			return failed("setting the proxy of %s: %v", svc, err)
		}
	}
	return nil
}
