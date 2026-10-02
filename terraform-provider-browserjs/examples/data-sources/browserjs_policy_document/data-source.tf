# Fill in forms on two sites: printable text only, a few keys, sane viewport
# sizes, no script.
data "browserjs_policy_document" "form_filling" {
  description      = "Fill in forms on two sites."
  allow_operations = ["click", "select", "wait", "screenshot", "url"]
  deny_operations  = ["evaluate", "setContent"]

  rule {
    operation = "navigate"
    constraint {
      parameter = "url"
      hosts     = ["forms.example.org", "*.intranet.example.org"]
    }
  }

  rule {
    operation = "type"
    constraint {
      parameter  = "text"
      max_length = 200
      pattern    = "^[\\x20-\\x7E]*$"
    }
  }

  rule {
    operation = "press"
    constraint {
      parameter = "key"
      allowed   = ["Enter", "Tab", "Escape"]
    }
  }

  rule {
    operation = "setViewport"
    constraint {
      parameter = "width"
      min       = 320
      max       = 1920
    }
    constraint {
      parameter = "height"
      min       = 200
      max       = 1200
    }
  }
}

resource "browserjs_session_policy" "forms" {
  session_id  = browserjs_session.forms.id
  managed_url = "https://github.com/example/infra/tree/main/browserjs"
  json        = data.browserjs_policy_document.form_filling.json
}
