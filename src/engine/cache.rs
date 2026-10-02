//! A small LRU over parsed rows, keyed by data-record index. Port of
//! internal/model/cache.go. Keeps scrolling around a multi-GB file cheap: only
//! the visible window plus recent history stays parsed in memory.
//!
//! The Go version uses container/list for O(1) move-to-front; here we use a
//! HashMap plus a VecDeque recording access order. Capacity is small (8192)
//! and Row() is only called interactively (not in Scan's hot loop), so the
//! O(cap) worst case per access is a non-issue in practice.

use std::collections::{HashMap, VecDeque};

pub struct RowCache {
    cap: usize,
    order: VecDeque<usize>, // front = most recently used
    items: HashMap<usize, Vec<String>>,
}

impl RowCache {
    pub fn new(capacity: usize) -> Self {
        let cap = capacity.max(1);
        Self {
            cap,
            order: VecDeque::with_capacity(cap),
            items: HashMap::with_capacity(cap),
        }
    }

    pub fn get(&mut self, key: usize) -> Option<Vec<String>> {
        if let Some(rec) = self.items.get(&key) {
            let rec = rec.clone();
            self.touch(key);
            Some(rec)
        } else {
            None
        }
    }

    pub fn put(&mut self, key: usize, rec: Vec<String>) {
        if self.items.contains_key(&key) {
            self.items.insert(key, rec);
            self.touch(key);
            return;
        }
        self.items.insert(key, rec);
        self.order.push_front(key);
        if self.order.len() > self.cap {
            if let Some(oldest) = self.order.pop_back() {
                self.items.remove(&oldest);
            }
        }
    }

    fn touch(&mut self, key: usize) {
        if let Some(pos) = self.order.iter().position(|&k| k == key) {
            self.order.remove(pos);
        }
        self.order.push_front(key);
    }
}
