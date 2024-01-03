# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0


job "elastic" {
  group "group" {
    scaling "no-label-allowed" {
      enabled = false
      min     = 5
      max     = 100

      policy {
        foo = "bar"
        b   = true
        val = 5
        f   = 0.1
      }
    }
  }
}
